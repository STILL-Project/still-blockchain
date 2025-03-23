package blockchain

import (
	"errors"
	"fmt"
	"still-blockchain/address"
	"still-blockchain/binary"
	"still-blockchain/block"
	"still-blockchain/config"
	"still-blockchain/logger"
	"still-blockchain/p2p"
	"still-blockchain/p2p/packet"
	"still-blockchain/stratum/stratumsrv"
	"still-blockchain/transaction"
	"still-blockchain/util"
	"still-blockchain/util/buck"
	"still-blockchain/util/uint128"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var Log = logger.New()

type Uint128 = uint128.Uint128

// Blockchain represents a Blockchain structure, for storing transactions
type Blockchain struct {
	DB      *bolt.DB
	P2P     *p2p.P2P
	Stratum *stratumsrv.Server

	shutdownInfo shutdownInfo

	Mining bool // locked by MergesMut

	Merges        []*mergestratum
	MergesMut     util.RWMutex
	mergesUpdated bool
	lastJob       time.Time

	BlockQueue *BlockQueue

	SyncHeight uint64  // top height seen from remote nodes
	SyncDiff   Uint128 // top cumulative diff seen from remote nodes
	SyncMut    util.RWMutex
}

func (bc *Blockchain) IsShuttingDown() bool {
	bc.shutdownInfo.RLock()
	defer bc.shutdownInfo.RUnlock()

	return bc.shutdownInfo.ShuttingDown
}

const FAST_SYNC = true

func New() *Blockchain {
	bc := &Blockchain{
		Stratum: &stratumsrv.Server{
			NewConnections: make(chan *stratumsrv.Conn),
		},
	}

	var err error
	bc.DB, err = bolt.Open("./"+config.NETWORK_NAME+".db", 0666, &bolt.Options{
		Timeout:        4 * time.Second,
		NoFreelistSync: true,
		NoSync:         FAST_SYNC,
	})
	if err != nil {
		panic(err)
	}

	bc.createBuck(buck.INFO)
	bc.createBuck(buck.BLOCK)
	bc.createBuck(buck.TOPO)
	bc.createBuck(buck.STATE)
	bc.createBuck(buck.TX)
	bc.createBuck(buck.INTX)
	bc.createBuck(buck.OUTTX)

	// add genesis block if it doesn't exist
	bc.addGenesis()

	var stats *Stats
	var mempool *Mempool
	bc.DB.View(func(tx *bolt.Tx) error {
		stats = bc.GetStats(tx)
		mempool = bc.GetMempool(tx)
		return nil
	})

	Log.Info("Started blockchain")
	Log.Infof("Height: %d", stats.TopHeight)
	Log.Infof("Cumulative diff: %.3fk\n", stats.CumulativeDiff.Float64()/1000)
	Log.Infof("Top hash: %x", stats.TopHash)
	Log.Debugf("Tips: %x", stats.Tips)
	Log.Debugf("Orphans: %x", stats.Orphans)
	Log.Debugf("Mempool: %d transactions", len(mempool.Entries))

	bc.SyncDiff = stats.CumulativeDiff
	bc.SyncHeight = stats.TopHeight

	bc.BlockQueue = NewBlockQueue(bc)

	if FAST_SYNC {
		go func() {
			// in case fast sync mode is enabled, we flush database to disk every minute
			for {
				time.Sleep(60 * time.Second)
				err = bc.DB.Sync()
				if err != nil {
					Log.Err("failed to sync database to disk:", err)
				}
			}
		}()
	}

	return bc
}

func (bc *Blockchain) Synchronize() {
	Log.Debug("Synchronization thread started")
	for {
		if bc.IsShuttingDown() {
			Log.Info("Synchronization thread stopped")
			return
		}

		var stats *Stats
		bc.DB.View(func(tx *bolt.Tx) error {
			stats = bc.GetStats(tx)
			return nil
		})

		bc.BlockQueue.Update(func(qt *QueueTx) {
			bc.fillQueue(qt, stats.TopHeight)

			reqbls := []*QueuedBlock{}
			for {
				reqbl := qt.RequestableBlock()
				if reqbl == nil {
					break
				}
				Log.Debugf("requesting block %d %x", reqbl.Height, reqbl.Hash)
				if reqbl.Height != 0 && reqbl.Height < stats.TopHeight {
					qt.RemoveBlockByHeight(reqbl.Height)
					continue
				}
				reqbls = append(reqbls, reqbl)
			}

			go func() {
				// TODO: shuffle P2P.Connections order
				for _, reqbl := range reqbls {
					for _, conn := range bc.P2P.Connections {
						sent := false
						conn.PeerData(func(d *p2p.PeerData) {
							if reqbl.Height == 0 || d.Stats.Height >= reqbl.Height {
								conn.SendPacket(&p2p.Packet{
									Type: packet.BLOCK_REQUEST,
									Data: packet.PacketBlockRequest{
										Height: reqbl.Height,
										Hash:   reqbl.Hash,
									}.Serialize(),
								})
								sent = true
							}
						})
						if sent {
							break
						}
					}
				}
			}()
		})

		time.Sleep(250 * time.Millisecond)
	}
}

// TODO: clean up expired queue

// Blockchain MUST be locked before calling this
func (bc *Blockchain) fillQueue(qt *QueueTx, topHeight uint64) {
	bc.SyncMut.RLock()
	syncHeight := bc.SyncHeight
	bc.SyncMut.RUnlock()

	if qt.Length() < config.PARALLEL_BLOCKS_DOWNLOAD {
		if syncHeight > topHeight {
			n := qt.Length()
			for i := topHeight + 1; i <= syncHeight; i++ {
				if n > config.PARALLEL_BLOCKS_DOWNLOAD {
					break
				}
				n++
				qt.SetBlock(NewQueuedBlock(i, [32]byte{}), false)
			}
		}
	}
}

type shutdownInfo struct {
	ShuttingDown bool
	sync.RWMutex
}

func (bc *Blockchain) Close() {
	bc.shutdownInfo.Lock()
	bc.shutdownInfo.ShuttingDown = true
	bc.shutdownInfo.Unlock()
	Log.Info("Stopping integrated miner if started")
	bc.MergesMut.Lock()
	bc.Mining = false
	bc.MergesMut.Unlock()
	Log.Info("Shutting down P2P server")
	bc.P2P.Close()
	Log.Info("Saving block download queue")
	bc.BlockQueue.Lock()
	bc.BlockQueue.Save()
	bc.BlockQueue.Unlock()
	if FAST_SYNC {
		Log.Info("Flushing database to disk")
		bc.DB.Sync()
	}
	Log.Info("Closing database")
	bc.DB.Close()
	Log.Info("STILL daemon shutdown complete. Bye!")
}

func (bc *Blockchain) addGenesis() {
	if util.Time() < config.GENESIS_TIMESTAMP {
		Log.Fatal("genesis block in future of", (config.GENESIS_TIMESTAMP-int64(util.Time()))/1000, "seconds")
	}

	genesis := &block.Block{
		BlockHeader: block.BlockHeader{
			Height:     0,
			Version:    0,
			Timestamp:  config.GENESIS_TIMESTAMP,
			Nonce:      0x1337,
			NonceExtra: [16]byte{},
			Recipient:  address.GenesisAddress,
			Ancestors:  block.Ancestors{},
		},
		Difficulty:     uint128.From64(1),
		CumulativeDiff: uint128.From64(1),
		Transactions:   []transaction.TXID{},
	}

	hash := genesis.Hash()

	Log.Debugf("genesis block hash is %x", hash)

	err := bc.DB.Update(func(tx *bolt.Tx) error {
		bl, err := bc.GetBlock(tx, hash)
		if err != nil {
			Log.Debug("genesis block is not in chain:", err)
			err := bc.insertBlockMain(tx, genesis)
			if err != nil {
				Log.Fatal(fmt.Errorf("failed adding genesis to chain: %v", err))
			}
			bc.SetStats(tx, &Stats{
				TopHash:        hash,
				TopHeight:      0,
				CumulativeDiff: genesis.Difficulty,
			})
			bc.SetMempool(tx, &Mempool{
				Entries: make([]*MempoolEntry, 0),
			})
			err = bc.ApplyBlockToState(tx, genesis, hash)
			if err != nil {
				return err
			}
		} else {
			if bl == nil {
				return errors.New("bl is nil")
			}
			Log.Debug("genesis is already in chain:", bl.String())
		}
		return nil
	})
	if err != nil {
		Log.Fatal(err)
	}
}

// checkBlock validates things like height, diff, etc. for a block. It doesn't validate PoW (that's done by
// bl.Prevalidate()) or transactions.
func (bc *Blockchain) checkBlock(tx *bolt.Tx, bl, prevBl *block.Block) error {
	// validate difficulty
	expectDiff, err := bc.GetNextDifficulty(tx, prevBl)
	if err != nil {
		err = fmt.Errorf("failed to get difficulty: %w", err)
		return err
	}
	if !bl.Difficulty.Equals(expectDiff) {
		return fmt.Errorf("block has invalid diff: %s, expected: %s", bl.Difficulty.String(),
			expectDiff.String())
	}

	// check that height is correct
	if bl.Height != prevBl.Height+1 {
		return fmt.Errorf("block has invalid height: %d, previous: %d", bl.Height, prevBl.Height)
	}

	// check that timestamp is strictly greater than previous block timestamp
	if prevBl.Timestamp > bl.Timestamp {
		return fmt.Errorf("block has timestamp that's older than previous block: %d<=%d", bl.Timestamp,
			prevBl.Timestamp)
	}

	// validate block's SideBlocks
	sideDiff := bl.Difficulty.Mul64(2 * uint64(len(bl.SideBlocks))).Div64(3)
	newCumDiff := prevBl.CumulativeDiff.Add(bl.Difficulty).Add(sideDiff)
	// since SideBlocks's Ancestors are derived from height, we don't have to check them here
	for _, side := range bl.SideBlocks {

		// check ancestors
		// TODO PRIORITY: audit this! It's of critical importance!
		var heightDiff int = -1 //
		for ancid, anc := range side.Ancestors {
			if heightDiff == -1 { // common not found
				// scan if we can find the ancestor
				for vid, v := range bl.Ancestors {
					if vid >= ancid && v == anc {
						heightDiff = vid - ancid
						Log.Debug("found ancestor at height difference:", heightDiff)
					}
				}
			} else { // common found, verify that subsequent blocks match
				if ancid+heightDiff >= len(bl.Ancestors) {
					break
				}
				if anc != bl.Ancestors[ancid+heightDiff] {
					return errors.New("subsequent block isn't valid")
				}
			}
		}
		if heightDiff == -1 {
			return fmt.Errorf("common block not found")
		}

		// check that the side block hasn't been already included
		if side.Equals(prevBl.Commitment()) {
			return fmt.Errorf("side block was already included (1)")
		}
		for _, v := range prevBl.SideBlocks { // first check in the prevBl, since we already obtained it
			if side.Equals(v) {
				return fmt.Errorf("side block was already included (2)")
			}
		}
		if prevBl.Height > 0 {
			for _, anc := range bl.Ancestors[1:] { // then check in previous ancestors
				ancBl, err := bc.GetBlock(tx, anc)
				if err != nil {
					Log.Err(err)
					return err
				}
				if side.Equals(ancBl.Commitment()) {
					return fmt.Errorf("side block was already included (3)")
				}
				for _, v := range ancBl.SideBlocks {
					if side.Equals(v) {
						return fmt.Errorf("side block was already included (4)")
					}
				}
				if ancBl.Height == 0 {
					break
				}
			}
		}
	}

	if !bl.CumulativeDiff.Equals(newCumDiff) {
		return fmt.Errorf("block has invalid cumulative diff: %s, expected: %s", bl.CumulativeDiff,
			newCumDiff)
	}

	return nil
}

// AddBlock attempts adding a block to the blockchain.
// Block should be already prevalidated.
// If the block doesn't fit in the mainchain, it is either added to an altchain or orphaned.
// Blockchain MUST be locked before calling this
func (bc *Blockchain) AddBlock(tx *bolt.Tx, bl *block.Block) (util.Hash, error) {
	hash := bl.Hash()

	// check if block is duplicate
	_, err := bc.GetBlock(tx, hash)
	if err == nil {
		return hash, fmt.Errorf("received duplicate block %x height %d", hash, bl.Height)
	}

	prevHash := bl.PrevHash()

	// check if block is orphaned
	prevBl, err := bc.GetBlock(tx, prevHash)
	if err != nil {
		err := bc.addOrphanBlock(tx, bl, hash, false)
		if err != nil {
			Log.Err(err)
			return hash, err
		}
		// mark orphan block as downloaded in queue
		bc.queuedBlockDownloaded(hash, bl.Height)
		return hash, nil
	}

	// check if parent block is orphaned
	stats := bc.GetStats(tx)
	if stats.Orphans[prevHash] != nil {
		// this block's parent is orphaned; add this block as an orphan
		err := bc.addOrphanBlock(tx, bl, hash, true)
		if err != nil {
			Log.Err(err)
			return hash, err
		}
		// mark orphan block as downloaded in queue
		bc.queuedBlockDownloaded(hash, bl.Height)
		return hash, nil
	}

	err = bc.checkBlock(tx, bl, prevBl)
	if err != nil {
		Log.Warn("block is invalid:", err)
		return hash, err
	}

	// add block to chain
	var isMainchain = prevHash == stats.TopHash
	if isMainchain {
		// remove block from queue
		bc.removeFromQueue(hash, bl.Height)

		err = bc.addMainchainBlock(tx, bl, hash)
	} else {
		bc.queuedBlockDownloaded(hash, bl.Height)

		err = bc.addAltchainBlock(tx, bl, hash)
	}
	if err != nil {
		Log.Err(err)
		return hash, err
	}
	err = bc.checkDeorphanage(tx, bl, hash)
	if err != nil {
		Log.Err(err)
		return hash, err
	}

	return hash, nil
}

func (bc *Blockchain) removeFromQueue(hash [32]byte, height uint64) {
	bc.BlockQueue.Update(func(qt *QueueTx) {
		qt.RemoveBlock(height, hash)
	})
}
func (bc *Blockchain) queuedBlockDownloaded(hash [32]byte, height uint64) {
	bc.BlockQueue.Update(func(qt *QueueTx) {
		qt.BlockDownloaded(height, hash)
	})
}

// addOrphanBlock should only be called by the addBlock method
// use parentKnown = true if this block has a known parent which is orphaned
// Blockchain MUST be locked before calling this
func (bc *Blockchain) addOrphanBlock(txn *bolt.Tx, bl *block.Block, hash [32]byte, parentKnown bool) error {
	Log.Infof("Adding orphan block %d %x diff: %s sides: %d parent known: %v", bl.Height, hash,
		bl.Difficulty, len(bl.SideBlocks), parentKnown)
	stats := bc.GetStats(txn)

	if stats.Orphans[hash] != nil {
		return errors.New("Orphan already exists! This should NEVER happen")
	}

	orphan := &Orphan{
		Expires:  time.Now().Add(time.Hour).Unix(), // orphan blocks expire after 1 hour
		Hash:     hash,
		PrevHash: bl.PrevHash(),
	}

	// add orphan prevhash to queued blocks, if it is not known already
	if !parentKnown {
		bc.BlockQueue.Update(func(qt *QueueTx) {
			qt.SetBlock(NewQueuedBlock(0, bl.PrevHash()), false)
		})
	}

	// TODO: clean up expired orphans

	// insert orphan
	stats.Orphans[hash] = orphan
	bc.setStatsNoBroadcast(txn, stats)

	return bc.insertBlock(txn, bl, hash)
}

// addAltchainBlock should only be called by the addBlock method
// Blockchain MUST be locked before calling this
func (bc *Blockchain) addAltchainBlock(txn *bolt.Tx, bl *block.Block, hash [32]byte) error {
	Log.Infof("Adding block as alternative on height: %d hash: %x diff: %s", bl.Height, hash, bl.Difficulty)
	stats := bc.GetStats(txn)

	// check if the block extends one of the tips
	extendTip := stats.Tips[bl.PrevHash()]

	if extendTip != nil {
		// block extends one of the tips, update that tip
		Log.Debugf("block %x extends tip %x", hash, extendTip.Hash)
		extendTip.Hash = hash
		extendTip.Height++
		extendTip.CumulativeDiff = bl.CumulativeDiff
	} else {
		// if the block doesn't extend tips, then it's creating a new tip
		Log.Debugf("new tip: %x", hash)
		stats.Tips[hash] = &AltchainTip{
			Hash:           hash,
			Height:         bl.Height,
			CumulativeDiff: bl.CumulativeDiff,
		}
	}

	// insert block and save stats
	err := bc.insertBlock(txn, bl, hash)
	if err != nil {
		Log.Err(err)
		return err
	}
	// broadcasting stats isn't necessary, altchain blocks don't affect our tophash
	bc.setStatsNoBroadcast(txn, stats)

	// check for reorgs
	bc.CheckReorgs(txn, stats)

	if bl.Height+config.MINIDAG_ANCESTORS >= stats.TopHeight {
		go bc.NewStratumJob(false)
	}

	return nil
}

// returns true if a reorg has happened
func (bc *Blockchain) CheckReorgs(tx *bolt.Tx, stats *Stats) (bool, error) {
	type hashInfo struct {
		Hash  [32]byte
		Block *block.Block
	}

	// Check if a reorg is needed
	var altDiff = stats.CumulativeDiff
	var altHash = stats.TopHash
	var altHeight = stats.TopHeight
	for _, v := range stats.Tips {
		if v.CumulativeDiff.Cmp(altDiff) > 0 {
			altDiff = v.CumulativeDiff
			altHash = v.Hash
			altHeight = v.Height
		}
	}
	// If the reorg is not needed, then return
	if altHash == stats.TopHash {
		Log.Debug("reorg not needed")
		return false, nil
	}
	Log.Infof("Reorg needed: height %d -> %d, hash %x, cumulative diff %s -> %s",
		stats.TopHeight, altHeight, altHash, stats.CumulativeDiff.String(), altDiff.String())

	// reorganize the chain
	err := func() error {
		// step 1: iterate the altchain blocks in reverse order to find out the common block with mainchain
		commonBlockHash := altHash
		commonBlock, err := bc.GetBlock(tx, commonBlockHash)
		if err != nil {
			Log.Err(err)
			return err
		}
		buckTopo := tx.Bucket([]byte{buck.TOPO})

		hashes := []hashInfo{
			{
				Hash:  commonBlockHash,
				Block: commonBlock,
			},
		} // hashes holds the altchain blocks, used in step 3

		// TODO: we can optimize this loop by scanning all of the block's known ancestors
		for {
			commonBlockHash = commonBlock.PrevHash()
			commonBlock, err = bc.GetBlock(tx, commonBlockHash)
			if err != nil {
				err := fmt.Errorf("reorg step 1: failed to get common block %x: %v", commonBlockHash, err)
				return err
			}
			Log.Debugf("reorg step 1: scanning altchain block %d %x", commonBlock.Height, commonBlockHash)

			if commonBlock.Height == 0 {
				err = errors.New("could not find common block")
				Log.Err(err)
				return err
			}

			topohash, err := bc.buckGetTopo(buckTopo, commonBlock.Height)
			// a block doesn't exist in mainchain at this height, just print the error and go on
			if err != nil {
				Log.Debug("a block doesn't exist in mainchain at this height (probably fine), err:", err)
			}

			if topohash == commonBlockHash {
				Log.Debugf("stopping just before block common: %x", commonBlockHash)
				break
			}

			hashes = append(hashes, hashInfo{
				Hash:  commonBlockHash,
				Block: commonBlock,
			})
		}

		// step 2: iterate the mainchain blocks in reverse order until common block to reverse the state
		// changes and remove the topoheight data (only do this if TopHash is not the common block's hash,
		// which can happen after a deorphanage)
		if stats.TopHash != commonBlockHash {
			nHash := stats.TopHash
			n, err := bc.GetBlock(tx, nHash)
			if err != nil {
				Log.Err(err)
				return err
			}

			if n.Hash() == commonBlockHash {
				Log.Debugf("reorg step 2 not needed")
			} else {
				for {
					if nHash == commonBlockHash {
						Log.Debugf("reorg step 2 done")
						break
					}
					if n.Height == 0 {
						err := fmt.Errorf("reorg: Block has height 0! Could not find common hash %x; nHash %x",
							commonBlockHash, nHash)
						Log.Err(err)
						return err
					}

					n, err = bc.GetBlock(tx, nHash)
					if err != nil {
						err := fmt.Errorf("failed to get block %x: %v", nHash, err)
						Log.Err(err)
						return err
					}

					Log.Debugf("reorg step 2: reversing changes of block %d %x", n.Height, nHash)

					// delete this block's topo
					heightBin := make([]byte, 8)
					binary.LittleEndian.PutUint64(heightBin, n.Height)
					err := buckTopo.Delete(heightBin)
					if err != nil {
						Log.Err(err)
						return err
					}

					// remove block from state
					err = bc.RemoveBlockFromState(tx, n, nHash)
					if err != nil {
						Log.Err(err)
						return err
					}

					nHash = n.PrevHash()
				}
			}
		}

		// step 3: iterate altchain blocks starting from common block to validate and apply them to the state
		// and to the topo; if any of these blocks is invalid, delete it and undo the reorg

		Log.Devf("hashes: %x", hashes)

		for i := len(hashes) - 1; i >= 0; i-- {
			Log.Devf("reorg step 3: setting topo: %d (height: %d) %x", i, hashes[i].Block.Height,
				hashes[i].Hash)

			// set this block's topo
			heightBin := make([]byte, 8)
			binary.LittleEndian.PutUint64(heightBin, hashes[i].Block.Height)
			err := buckTopo.Put(heightBin, hashes[i].Hash[:])
			if err != nil {
				Log.Err(err)
				return err
			}

			bl := hashes[i].Block

			// set the block's cumulative difficulty
			prevBl, err := bc.GetBlock(tx, bl.PrevHash())
			if err != nil {
				Log.Err(err)
				return err
			}

			err = bc.checkBlock(tx, bl, prevBl)
			if err != nil {
				Log.Warn("reorg invalid block:", err)
				return err
			}

			err = bc.ApplyBlockToState(tx, bl, hashes[i].Hash)
			if err != nil {
				Log.Err(err)
				return err
			}

			bc.BlockQueue.Update(func(qt *QueueTx) {
				qt.RemoveBlockByHeight(bl.Height)
			})
		}

		// step 4: update the stats
		Log.Devf("starting reorg step 4")

		infoBuck := tx.Bucket([]byte{buck.INFO})
		stats = bc.GetStats(tx)

		// add the old mainchain as an altchain tip
		delete(stats.Tips, altHash)
		stats.Tips[stats.TopHash] = &AltchainTip{
			Hash:           stats.TopHash,
			Height:         stats.TopHeight,
			CumulativeDiff: stats.CumulativeDiff,
		}

		// set the new mainchain
		stats.TopHash = altHash
		stats.CumulativeDiff = altDiff
		stats.TopHeight = altHeight

		infoBuck.Put([]byte("stats"), stats.Serialize())

		Log.Infof("Reorganize success, new height: %d hash: %x cumulative diff: %s", stats.TopHeight,
			stats.TopHash, stats.CumulativeDiff)
		return nil
	}()

	if err != nil {
		Log.Err("Reorg failed:", err)
		return false, err
	}
	return true, nil
}

// addMainchainBlock should only be called by the addBlock method
// Blockchain MUST be locked before calling this
func (bc *Blockchain) addMainchainBlock(tx *bolt.Tx, bl *block.Block, hash [32]byte) error {
	err := bc.ApplyBlockToState(tx, bl, hash)
	if err != nil {
		Log.Warn("block is invalid, not adding to mainchain:", err)
		return err
	}

	Log.Infof("Adding mainchain block %d %x diff: %s sides: %d", bl.Height, hash, bl.Difficulty, len(bl.SideBlocks))
	stats := bc.GetStats(tx)

	stats.TopHash = hash
	stats.TopHeight = bl.Height
	stats.CumulativeDiff = bl.CumulativeDiff
	bc.SetStats(tx, stats)

	// add block to mainchain and update stats
	err = bc.insertBlockMain(tx, bl)
	if err != nil {
		Log.Err(err)
		return err
	}

	Log.Debugf("done adding block %x to mainchain", hash)

	return nil
}

// Validates a block, and then adds it to the state
func (bc *Blockchain) ApplyBlockToState(txn *bolt.Tx, bl *block.Block, _ [32]byte) error {
	bstate := txn.Bucket([]byte{buck.STATE})

	// remove transactions from mempool
	bst := txn.Bucket([]byte{buck.INFO})
	pool := bc.buckGetMempool(bst)
	for _, t := range bl.Transactions {
		pool.DeleteEntry(t)
	}
	bc.buckSetMempool(bst, pool)

	var totalFee uint64 = 0

	// validate and apply transactions
	btx := txn.Bucket([]byte{buck.TX})
	for _, v := range bl.Transactions {
		tx, _, err := bc.buckGetTx(btx, v)
		if err != nil {
			Log.Err(err)
			return err
		}
		senderAddr := address.FromPubKey(tx.Sender)

		Log.Debugf("Applying transaction %x to mainchain; sender: %s, recipient: %s", v,
			address.FromPubKey(tx.Sender), tx.Recipient)

		// check sender state
		senderState, err := bc.buckGetState(bstate, senderAddr)
		if err != nil {
			Log.Err(err)
			return err
		}
		Log.Dev("sender state before:", senderState)

		if senderState.Balance < tx.Amount+tx.Fee {
			err = fmt.Errorf("transaction %x spends too much money: balance: %d, amount: %d, fee: %d", v,
				senderState.Balance, tx.Amount, tx.Fee)
			Log.Warn(err)
			return err
		}
		if tx.Nonce != senderState.LastNonce+1 {
			err = fmt.Errorf("transaction %x has unexpected nonce: %d, previous nonce: %d", v,
				tx.Nonce, senderState.LastNonce)
			Log.Warn(err)
			return err
		}

		// apply sender state
		senderState.Balance -= tx.Amount + tx.Fee
		senderState.LastNonce++
		err = bc.buckSetState(bstate, senderAddr, senderState)
		if err != nil {
			Log.Err(err)
			return err
		}

		Log.Dev("sender state after:", senderState)

		// add the funds to recipient
		recState, err := bc.buckGetState(bstate, tx.Recipient)
		if err != nil {
			Log.Debug("recipient state not previously known:", err)
			recState = &State{
				Balance: 0, LastNonce: 0,
			}
		}
		Log.Devf("recipient %s state before: %v", tx.Recipient, recState)

		recState.Balance += tx.Amount
		recState.LastIncoming++ // also increase recipient's LastIncoming

		Log.Devf("recipient %s state after: %v", tx.Recipient, recState)

		// add tx hash to recipient's incoming list
		err = bc.SetTxTopoInc(txn, v, tx.Recipient, recState.LastIncoming)
		if err != nil {
			Log.Err(err)
			return err
		}
		// add tx hash to sender's outgoing list
		err = bc.SetTxTopoOut(txn, v, senderAddr, senderState.LastNonce)
		if err != nil {
			Log.Err(err)
			return err
		}
		// update tx height
		err = bc.SetTxHeight(txn, v, bl.Height)
		if err != nil {
			Log.Err(err)
			return err
		}

		err = bc.buckSetState(bstate, tx.Recipient, recState)
		if err != nil {
			Log.Err(err)
			return err
		}

		// apply tx to total fee
		totalFee += tx.Fee
	}

	// add block reward to coinbase transaction
	{
		totalReward := bl.Reward() + totalFee
		governanceReward := totalReward * config.BLOCK_REWARD_FEE_PERCENT / 100
		minerReward := totalReward - governanceReward

		Log.Debug("adding block reward", totalReward, "miner:", minerReward, "governance:", governanceReward)

		// apply miner reward
		minerState, err := bc.buckGetState(bstate, bl.Recipient)
		if err != nil {
			Log.Debugf("coinbase reward account not previously known: %s", err)
		}
		minerState.Balance += minerReward
		minerState.LastIncoming++
		err = bc.buckSetState(bstate, bl.Recipient, minerState)
		if err != nil {
			Log.Err(err)
			return err
		}
		// add block hash to recipient's incoming list
		err = bc.SetTxTopoInc(txn, bl.Hash(), bl.Recipient, minerState.LastIncoming)
		if err != nil {
			Log.Err(err)
			return err
		}

		// apply governance reward
		governanceState, err := bc.buckGetState(bstate, address.GenesisAddress)
		if err != nil {
			Log.Debugf("governance reward account not previously known: %s", err)
		}
		governanceState.Balance += governanceReward
		err = bc.buckSetState(bstate, address.GenesisAddress, governanceState)
		if err != nil {
			Log.Err(err)
			return err
		}
		// governance reward transactions aren't saved in incoming tx list
	}

	// update some stats
	bc.SyncMut.Lock()
	if bc.SyncDiff.Cmp(bl.CumulativeDiff) < 0 {
		bc.SyncHeight = bl.Height
		bc.SyncDiff = bl.CumulativeDiff
	}
	bc.SyncMut.Unlock()

	return nil
}

// Reverses the transaction of a block from the blockchain state
func (bc *Blockchain) RemoveBlockFromState(txn *bolt.Tx, bl *block.Block, blhash [32]byte) error {
	bstate := txn.Bucket([]byte{buck.STATE})
	btx := txn.Bucket([]byte{buck.TX})

	// TODO: add removed transactions to mempool

	type txCache struct {
		Hash [32]byte
		Tx   *transaction.Transaction
	}
	txs := make([]txCache, len(bl.Transactions))

	// iterate transactions to find tx fee sum for coinbase transaction
	var totalFee uint64
	for _, v := range bl.Transactions {
		tx, _, err := bc.buckGetTx(btx, v)
		if err != nil {
			Log.Err(err)
			return err
		}
		totalFee += tx.Fee
		txs = append(txs, txCache{
			Hash: v,
			Tx:   tx,
		})
	}

	// undo coinbase transaction
	{
		totalReward := bl.Reward() + totalFee
		governanceReward := totalReward * config.BLOCK_REWARD_FEE_PERCENT / 100
		minerReward := totalReward - governanceReward

		Log.Debug("removing block reward", totalReward, "miner:", minerReward, "governance:", governanceReward)

		// undo miner transaction
		minerState, err := bc.buckGetState(bstate, bl.Recipient)
		if err != nil {
			err := fmt.Errorf("coinbase reward account unknown: %s", err)
			Log.Err(err)
			return err
		}
		if minerState.Balance < minerReward {
			err := fmt.Errorf("balance of coinbase account is too small! balance: %d, block reward: %d",
				minerState.Balance, minerReward)
			Log.Err(err)
			return err
		}
		if minerState.LastIncoming == 0 {
			err = fmt.Errorf("coinbase %s LastIncoming must not be zero in block %x", bl.Recipient, blhash)
			Log.Err(err)
			return err
		}
		minerState.Balance -= minerReward
		minerState.LastIncoming--
		err = bc.buckSetState(bstate, bl.Recipient, minerState)
		if err != nil {
			Log.Err(err)
			return err
		}
		// removing coinbase transaction from incoming tx list is not necessary - since it's never read, and
		// later overwritten

		// undo governance reward
		governanceState, err := bc.buckGetState(bstate, address.GenesisAddress)
		if err != nil {
			err := fmt.Errorf("coinbase reward account unknown: %s", err)
			Log.Err(err)
			return err
		}
		if governanceState.Balance < governanceReward {
			err := fmt.Errorf("balance of coinbase account is too small! balance: %d, block reward: %d",
				governanceState.Balance, governanceReward)
			Log.Err(err)
			return err
		}
		governanceState.Balance -= governanceReward
		err = bc.buckSetState(bstate, address.GenesisAddress, governanceState)
		if err != nil {
			Log.Err(err)
			return err
		}
		// governance reward transactions aren't saved in incoming tx list
	}

	// remove transactions in reverse order
	for i := len(txs) - 1; i >= 0; i-- {
		tx := txs[i].Tx
		txhash := txs[i].Hash

		Log.Devf("removing transaction %x (index %d) from state", txhash, i)

		senderAddr := address.FromPubKey(tx.Sender)

		// decrease recipient balance and LastIncoming
		{
			recState, err := bc.GetState(txn, tx.Recipient)
			if err != nil {
				Log.Err(err)
				return err
			}
			if recState.Balance < tx.Amount+tx.Fee {
				err := fmt.Errorf("recipient balance is smaller than tx amount + fee: %d < %d+%d",
					recState.Balance, tx.Amount, tx.Fee)
				if err != nil {
					Log.Err(err)
					return err
				}
			}
			if recState.LastIncoming == 0 {
				err = fmt.Errorf("recipient %s LastIncoming must not be zero in tx %x", tx.Recipient, txhash)
				Log.Err(err)
				return err
			}
			recState.Balance -= tx.Amount
			recState.LastIncoming--
			err = bc.SetState(txn, tx.Recipient, recState)
			if err != nil {
				Log.Err(err)
				return err
			}
		}

		// increase sender balance and decrease nonce
		{
			senderState, err := bc.GetState(txn, senderAddr)
			if err != nil {
				Log.Err(err)
				return err
			}
			if senderState.LastNonce == 0 {
				err = fmt.Errorf("sender %s last nonce must not be zero in tx %x", senderAddr, txhash)
				Log.Err(err)
				return err
			}
			senderState.Balance += tx.Amount
			senderState.Balance += tx.Fee
			senderState.LastNonce--
			err = bc.SetState(txn, senderAddr, senderState)
			if err != nil {
				Log.Err(err)
				return err
			}
		}

		// set tx height to zero
		err := bc.SetTxHeight(txn, txhash, bl.Height)
		if err != nil {
			Log.Err(err)
			return err
		}

	}

	return nil
}

func (bc *Blockchain) GetState(tx *bolt.Tx, addr address.Address) (s *State, err error) {
	b := tx.Bucket([]byte{buck.STATE})
	return bc.buckGetState(b, addr)
}
func (bc *Blockchain) buckGetState(b *bolt.Bucket, addr address.Address) (*State, error) {
	var s = &State{}
	bin := b.Get(addr[:])
	if bin == nil {
		return s, fmt.Errorf("address %s not in state", addr)
	}
	err := s.Deserialize(bin)
	return s, err
}
func (bc *Blockchain) SetState(tx *bolt.Tx, addr address.Address, state *State) (err error) {
	b := tx.Bucket([]byte{buck.STATE})
	return bc.buckSetState(b, addr, state)
}
func (bc *Blockchain) buckSetState(b *bolt.Bucket, addr address.Address, state *State) error {
	return b.Put(addr[:], state.Serialize())
}

func (bc *Blockchain) CreateCheckpoints(tx *bolt.Tx, maxHeight, interval uint64) ([]byte, error) {
	s := binary.NewSer(make([]byte, maxHeight/interval*32))
	s.AddUint32(uint32(interval))
	for height := interval; height <= maxHeight; height += interval {
		bl, err := bc.GetTopo(tx, height)
		if err != nil {
			Log.Err(err)
			return nil, err
		}
		Log.Devf("Adding block %d %x to checkpoints", height, bl)
		s.AddFixedByteArray(bl[:])
	}
	return s.Output(), nil
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) checkDeorphanage(tx *bolt.Tx, bl *block.Block, hash [32]byte) error {
	Log.Debugf("checkDeorphanage %x", hash)
	stats := bc.GetStats(tx)

	// no need to remove block from queue, it's removed by parent of this function

	// recursively check for deorphans
	err := bc.deorphanBlock(tx, bl, hash, stats)
	if err != nil {
		Log.Err(err)
		return err
	}

	// finally, save stats
	bc.SetStats(tx, stats)

	// now that blocks were deorphaned, there might be a reorg
	reorg, err := bc.CheckReorgs(tx, stats)
	if err != nil {
		Log.Err(err)
		return err
	}
	if reorg {
		stats = bc.GetStats(tx)
		bc.cleanupTips(tx, stats)
		bc.SetStats(tx, stats)
	}

	return nil
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) cleanupTips(tx *bolt.Tx, stats *Stats) {
	Log.Debug("cleaning up tips")
	for i, tip := range stats.Tips {
		topo, err := bc.GetTopo(tx, tip.Height)
		if err != nil {
			Log.Debugf("cleanupTips error is %v; this is probably fine", err)
			continue
		}
		if topo == tip.Hash {
			Log.Debugf("cleanupTips: tip %x is included in mainchain, discarding it", tip.Hash)
			delete(stats.Tips, i)
		}
	}
}

// recursive function which finds all the orphans that are children of the given hash, and creates altchain
// don't forget to save stats later, as this function doesn't do that
func (bc *Blockchain) deorphanBlock(tx *bolt.Tx, prev *block.Block, prevHash [32]byte, stats *Stats) error {
	Log.Debugf("deorphanBlock hash %x", prevHash)

	for i, v := range stats.Orphans {
		if v.PrevHash == prevHash {
			Log.Debugf("deorphanBlock: %x is deorphaning %x", prevHash, v.Hash)
			bl, err := bc.GetBlock(tx, v.Hash)
			h2 := v.Hash
			if err != nil {
				Log.Err(err)
				return err
			}

			// Here we don't fully validate the block, as we don't know the current state. Instead we only
			// update the cumulative difficulty, as it's needed for the tips
			cdiff := prev.CumulativeDiff.Add(bl.Difficulty)
			sideDiff := bl.Difficulty.Mul64(uint64(len(bl.SideBlocks)) * 2).Div64(3)
			cdiff = cdiff.Add(sideDiff)

			if !cdiff.Equals(bl.CumulativeDiff) {
				Log.Devf("deorphanBlock: block cumulative difficulty updated: %s -> %s", bl.CumulativeDiff,
					cdiff)
				bl.CumulativeDiff = cdiff
				bc.insertBlock(tx, bl, h2)
			}

			// remove this block from orphans
			delete(stats.Orphans, i)

			// remove bl's tip
			delete(stats.Tips, prevHash)
			// add bl2 to tips
			stats.Tips[h2] = &AltchainTip{
				Hash:           h2,
				Height:         bl.Height,
				CumulativeDiff: bl.CumulativeDiff,
			}

			// recall this function to find bl2's children
			bc.deorphanBlock(tx, bl, h2, stats)
		}
	}

	return nil
}

// Blockchain MUST be RLocked before calling this
func (bc *Blockchain) GetStats(tx *bolt.Tx) *Stats {
	b := tx.Bucket([]byte{buck.INFO})

	d := b.Get([]byte("stats"))

	if len(d) == 0 {
		Log.Fatal("stats are empty")
	}

	s, err := DeserializeStats(d)
	if err != nil {
		Log.Fatal(err)
	}

	return s
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) SetStats(tx *bolt.Tx, s *Stats) {
	if s.TopHeight != 0 {
		go bc.SendStats(s)
	}
	bc.setStatsNoBroadcast(tx, s)
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) setStatsNoBroadcast(tx *bolt.Tx, s *Stats) {
	b := tx.Bucket([]byte{buck.INFO})
	err := b.Put([]byte("stats"), s.Serialize())
	if err != nil {
		Log.Fatal(err)
	}
}

// Blockchain MUST be RLocked before calling this
func (bc *Blockchain) GetMempool(tx *bolt.Tx) *Mempool {
	b := tx.Bucket([]byte{buck.INFO})
	s, err := DeserializeMempool(b.Get([]byte("mempool")))
	if err != nil {
		Log.Fatal(err)
	}
	return s
}

// Blockchain MUST be RLocked before calling this
func (bc *Blockchain) buckGetMempool(b *bolt.Bucket) *Mempool {
	s, err := DeserializeMempool(b.Get([]byte("mempool")))
	if err != nil {
		Log.Fatal(err)
	}
	return s
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) SetMempool(tx *bolt.Tx, s *Mempool) {
	b := tx.Bucket([]byte{buck.INFO})
	err := b.Put([]byte("mempool"), s.Serialize())
	if err != nil {
		Log.Fatal(err)
	}
}

// Blockchain MUST be locked before calling this
func (bc *Blockchain) buckSetMempool(b *bolt.Bucket, s *Mempool) {
	err := b.Put([]byte("mempool"), s.Serialize())
	if err != nil {
		Log.Fatal(err)
	}
}

// insertBlockMain inserts a block to the blockchain, updating topoheight and removing its transactions from
// mempool (if applicable).
// This should be only called if you are sure that the block extends mainchain.
// Blockchain MUST be locked before calling this
func (bc *Blockchain) insertBlockMain(tx *bolt.Tx, bl *block.Block) error {
	hash := bl.Hash()

	defer func() {
		go bc.NewStratumJob(true)
	}()

	// add block data
	b := tx.Bucket([]byte{buck.BLOCK})
	err := b.Put(hash[:], bl.Serialize())
	if err != nil {
		return err
	}

	// add block topo
	b = tx.Bucket([]byte{buck.TOPO})
	heightBin := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBin, bl.Height)
	return b.Put(heightBin, hash[:])
}

// insertBlock inserts a block to the blockchain, without updating topoheight.
// Blockchain MUST be locked before calling this
func (bc *Blockchain) insertBlock(tx *bolt.Tx, bl *block.Block, hash [32]byte) error {
	// add block data
	b := tx.Bucket([]byte{buck.BLOCK})

	err := b.Put(hash[:], bl.Serialize())
	if err != nil {
		Log.Err(err)
		return err
	}

	blData := b.Get(hash[:])
	if len(blData) < 1 {
		return errors.New("blData is empty")
	}
	return nil
}

// GetBlock returns the block given its hash
// Blockchain MUST be RLocked before calling this
func (bc *Blockchain) GetBlock(tx *bolt.Tx, hash [32]byte) (*block.Block, error) {
	bl := &block.Block{}
	// read block data
	b := tx.Bucket([]byte{buck.BLOCK})
	blbin := b.Get(hash[:])
	if len(blbin) == 0 {
		return bl, fmt.Errorf("block %x not found", hash)
	}
	err := bl.Deserialize(blbin)
	return bl, err
}

func (bc *Blockchain) GetTopo(tx *bolt.Tx, height uint64) ([32]byte, error) {
	var blHash [32]byte
	b := tx.Bucket([]byte{buck.TOPO})
	heightBin := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBin, height)
	topoHash := b.Get(heightBin)
	if len(topoHash) != 32 {
		return blHash, errors.New("unknown block")
	}
	blHash = [32]byte(topoHash)
	return blHash, nil
}
func (bc *Blockchain) buckGetTopo(buck *bolt.Bucket, height uint64) ([32]byte, error) {
	var blHash [32]byte

	heightBin := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBin, height)

	topoHash := buck.Get(heightBin)

	if len(topoHash) != 32 {
		return [32]byte{}, errors.New("unknown block")
	}

	blHash = [32]byte(topoHash)

	return blHash, nil
}
func (bc *Blockchain) GetBlockByHeight(tx *bolt.Tx, height uint64) (*block.Block, error) {
	hash, err := bc.GetTopo(tx, height)
	if err != nil {
		return nil, err
	}
	return bc.GetBlock(tx, hash)
}

func (bc *Blockchain) StartP2P(peers []string, port uint16) {
	p2p.Log = Log
	bc.P2P = p2p.Start(peers)
	bc.P2P.StartClients()

	go bc.pinger()
	go bc.incomingP2P()
	go bc.newConnections()
	go bc.Synchronize()

	bc.P2P.ListenServer(port)
}

func (bc *Blockchain) GetSupply(tx *bolt.Tx) uint64 {
	var sum uint64 = 0
	b := tx.Bucket([]byte{buck.STATE})

	err := b.ForEach(func(k, v []byte) error {
		state := &State{}
		err := state.Deserialize(v)
		if err != nil {
			Log.Warn(address.Address(k), err)
		}
		sum += state.Balance
		return nil
	})
	if err != nil {
		Log.Err(err)
	}
	return sum
}
func (bc *Blockchain) CheckSupply(tx *bolt.Tx) {
	sum := bc.GetSupply(tx)
	supply := block.GetSupplyAtHeight(bc.GetStats(tx).TopHeight)
	if sum != supply {
		err := fmt.Errorf("invalid supply %d, expected %d", sum, supply)
		Log.Fatal(err)
	}
	Log.Debug("CheckSupply: supply is correct:", sum)
}

func (bc *Blockchain) SetTxTopoInc(tx *bolt.Tx, txid [32]byte, addr address.Address, incid uint64) error {
	incbin := addr[:]
	incbin = binary.AppendUvarint(incbin, incid)
	b := tx.Bucket([]byte{buck.INTX})
	return b.Put(incbin, txid[:])
}
func (bc *Blockchain) SetTxTopoOut(tx *bolt.Tx, txid [32]byte, addr address.Address, outid uint64) error {
	outbin := addr[:]
	outbin = binary.AppendUvarint(outbin, outid)
	b := tx.Bucket([]byte{buck.OUTTX})
	return b.Put(outbin, txid[:])
}

func (bc *Blockchain) GetTxTopoInc(tx *bolt.Tx, addr address.Address, incid uint64) ([32]byte, error) {
	incbin := addr[:]
	incbin = binary.AppendUvarint(incbin, incid)
	b := tx.Bucket([]byte{buck.INTX})
	bin := b.Get(incbin)
	if len(bin) != 32 {
		return [32]byte{}, errors.New("unknown tx topo inc")
	}
	return [32]byte(bin), nil
}
func (bc *Blockchain) GetTxTopoOut(tx *bolt.Tx, addr address.Address, outid uint64) ([32]byte, error) {
	outbin := addr[:]
	outbin = binary.AppendUvarint(outbin, outid)
	b := tx.Bucket([]byte{buck.OUTTX})
	bin := b.Get(outbin)
	if len(bin) != 32 {
		return [32]byte{}, errors.New("unknown tx topo out")
	}
	return [32]byte(bin), nil
}

func (bc *Blockchain) createBuck(name byte) {
	bc.DB.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte{name})
		if err != nil {
			return fmt.Errorf("createBuck: %s", err)
		}
		return nil
	})
}
