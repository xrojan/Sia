package contractor

import (
	"errors"
	"net"
	"time"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

var (
	// sectorHeight is the height of a Merkle tree that covers a single
	// sector. It is log2(modules.SectorSize / crypto.SegmentSize)
	sectorHeight = func() uint64 {
		height := uint64(0)
		for 1<<height < (modules.SectorSize / crypto.SegmentSize) {
			height++
		}
		return height
	}()
)

// An Editor modifies a Contract by communicating with a host. It uses the
// contract revision protocol to send modification requests to the host.
// Editors are the means by which the renter uploads data to hosts.
type Editor interface {
	// Upload revises the underlying contract to store the new data. It
	// returns the Merkle root of the data.
	Upload(data []byte) (root crypto.Hash, err error)

	// Delete removes a sector from the underlying contract.
	Delete(crypto.Hash) error

	// Address returns the address of the host.
	Address() modules.NetAddress

	// ContractID returns the FileContractID of the contract.
	ContractID() types.FileContractID

	// EndHeight returns the height at which the contract ends.
	EndHeight() types.BlockHeight

	// Close terminates the connection to the host.
	Close() error
}

// A hostEditor modifies a Contract by calling the revise RPC on a host. It
// implements the Editor interface. hostEditors are NOT thread-safe; calls to
// Upload must happen in serial.
type hostEditor struct {
	// constants
	host modules.HostDBEntry

	// updated after each revision
	contract Contract

	// resources
	conn       net.Conn
	contractor *Contractor
}

// Address returns the NetAddress of the host.
func (he *hostEditor) Address() modules.NetAddress { return he.contract.IP }

// ContractID returns the ID of the contract being revised.
func (he *hostEditor) ContractID() types.FileContractID { return he.contract.ID }

// EndHeight returns the height at which the host is no longer obligated to
// store the file.
func (he *hostEditor) EndHeight() types.BlockHeight { return he.contract.FileContract.WindowStart }

// Close cleanly ends the revision process with the host and closes the
// connection.
func (he *hostEditor) Close() error {
	// don't care about these errors
	_, _ = verifySettings(he.conn, he.host, he.contractor.hdb)
	_ = modules.WriteNegotiationStop(he.conn)
	return he.conn.Close()
}

// startRevision is run at the beginning of each revision iteration. It reads
// the host's settings confirms that the values are acceptable, and writes an acceptance.
func (he *hostEditor) startRevision() error {
	// verify the host's settings and confirm its identity
	// TODO: return new host, so we can calculate price accurately
	_, err := verifySettings(he.conn, he.host, he.contractor.hdb)
	if err != nil {
		return modules.WriteNegotiationRejection(he.conn, err)
	}
	return modules.WriteNegotiationAcceptance(he.conn)
}

// Upload revises an existing file contract with a host, and then uploads a
// piece to it.
func (he *hostEditor) Upload(data []byte) (crypto.Hash, error) {
	// calculate price
	he.contractor.mu.RLock()
	height := he.contractor.blockHeight
	he.contractor.mu.RUnlock()
	if height >= he.contract.FileContract.WindowStart {
		return crypto.Hash{}, errors.New("contract has already ended")
	}
	sectorPrice := he.host.StoragePrice.Mul(types.NewCurrency64(modules.SectorSize * uint64(he.contract.FileContract.WindowStart-height)))

	// calculate the Merkle root of the new data (no error possible with bytes.Reader)
	pieceRoot := crypto.MerkleRoot(data)

	// calculate the new total Merkle root
	newRoots := append(he.contract.MerkleRoots, pieceRoot)
	tree := crypto.NewCachedTree(sectorHeight) // NOTE: height is not strictly necessary here
	for _, h := range newRoots {
		tree.Push(h)
	}
	merkleRoot := tree.Root()

	// initiate revision
	err := he.startRevision()
	if err != nil {
		return crypto.Hash{}, err
	}

	// send 'insert' action
	err = encoding.WriteObject(he.conn, []modules.RevisionAction{{
		Type:        modules.ActionInsert,
		SectorIndex: uint64(len(he.contract.MerkleRoots)),
		Data:        data,
	}})
	if err != nil {
		return crypto.Hash{}, err
	}

	// revise the file contract to cover the cost of the new sector
	// TODO: should probably create revision beforehand so we know we have enough money
	rev := newRevision(he.contract.LastRevision, merkleRoot, uint64(len(newRoots)), sectorPrice)
	signedTxn, err := negotiateRevision(he.conn, rev, he.contract.SecretKey, height)
	if err != nil {
		return crypto.Hash{}, err
	}

	// update host contract
	he.contract.LastRevision = rev
	he.contract.LastRevisionTxn = signedTxn
	he.contract.MerkleRoots = newRoots

	he.contractor.mu.Lock()
	he.contractor.contracts[he.contract.ID] = he.contract
	he.contractor.save()
	he.contractor.mu.Unlock()

	return pieceRoot, nil
}

// Delete deletes a sector in a contract.
func (he *hostEditor) Delete(root crypto.Hash) error {
	// calculate price
	he.contractor.mu.RLock()
	height := he.contractor.blockHeight
	he.contractor.mu.RUnlock()
	if height >= he.contract.FileContract.WindowStart {
		return errors.New("contract has already ended")
	}
	// TODO: is this math correct?
	sectorPrice := he.host.StoragePrice.Mul(types.NewCurrency64(modules.SectorSize * uint64(he.contract.FileContract.WindowStart-height)))

	// calculate the new total Merkle root
	var newRoots []crypto.Hash
	index := -1
	for i, h := range he.contract.MerkleRoots {
		if h != root {
			index = i
		} else {
			newRoots = append(newRoots, h)
		}
	}
	if index == -1 {
		return errors.New("no record of that sector root")
	}
	tree := crypto.NewCachedTree(sectorHeight) // NOTE: height is not strictly necessary here
	for _, h := range newRoots {
		tree.Push(h)
	}
	merkleRoot := tree.Root()

	// initiate revision
	err := he.startRevision()
	if err != nil {
		return err
	}

	// send 'delete' action
	err = encoding.WriteObject(he.conn, []modules.RevisionAction{{
		Type:        modules.ActionDelete,
		SectorIndex: uint64(index),
	}})
	if err != nil {
		return err
	}

	// revise the file contract to cover one fewer sector
	rev := newRevision(he.contract.LastRevision, merkleRoot, uint64(len(newRoots)), sectorPrice)
	signedTxn, err := negotiateRevision(he.conn, rev, he.contract.SecretKey, height)
	if err != nil {
		return err
	}

	// update host contract
	he.contract.LastRevision = rev
	he.contract.LastRevisionTxn = signedTxn
	he.contract.MerkleRoots = newRoots

	he.contractor.mu.Lock()
	he.contractor.contracts[he.contract.ID] = he.contract
	he.contractor.save()
	he.contractor.mu.Unlock()

	return nil
}

// Editor initiates the contract revision process with a host, and returns
// an Editor.
func (c *Contractor) Editor(contract Contract) (Editor, error) {
	c.mu.RLock()
	height := c.blockHeight
	c.mu.RUnlock()
	if height > contract.FileContract.WindowStart {
		return nil, errors.New("contract has already ended")
	}
	host, ok := c.hdb.Host(contract.IP)
	if !ok {
		return nil, errors.New("no record of that host")
	}
	if host.StoragePrice.Cmp(maxPrice) > 0 {
		return nil, errTooExpensive
	}

	// check that contract has enough value to support an upload
	if len(contract.LastRevision.NewValidProofOutputs) != 2 {
		return nil, errors.New("invalid contract")
	}
	if !host.StoragePrice.IsZero() {
		bytes, errOverflow := contract.LastRevision.NewValidProofOutputs[0].Value.Div(host.StoragePrice).Uint64()
		if errOverflow == nil && bytes < modules.SectorSize {
			return nil, errors.New("contract has insufficient capacity")
		}
	}

	// initiate revision loop
	conn, err := c.dialer.DialTimeout(contract.IP, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if err := encoding.WriteObject(conn, modules.RPCReviseContract); err != nil {
		return nil, errors.New("couldn't initiate RPC: " + err.Error())
	}
	// send contract ID
	if err := encoding.WriteObject(conn, contract.ID); err != nil {
		return nil, errors.New("couldn't send contract ID: " + err.Error())
	}
	// read acceptance
	if err := modules.ReadNegotiationAcceptance(conn); err != nil {
		return nil, errors.New("host did not accept revision request: " + err.Error())
	}
	// read last txn
	var lastRevisionTxn types.Transaction
	if err := encoding.ReadObject(conn, &lastRevisionTxn, 2048); err != nil {
		return nil, errors.New("couldn't read last revision transaction: " + err.Error())
	} else if lastRevisionTxn.ID() != contract.LastRevisionTxn.ID() {
		return nil, errors.New("desynchronized with host (revision transactions do not match)")
	}

	// the host is now ready to accept revisions
	he := &hostEditor{
		contract: contract,
		host:     host,

		conn:       conn,
		contractor: c,
	}

	return he, nil
}
