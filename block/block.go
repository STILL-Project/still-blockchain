package block

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"still-blockchain/address"
	"still-blockchain/binary"
	"still-blockchain/checkpoints"
	"still-blockchain/config"
	"still-blockchain/transaction"
	"still-blockchain/util"
	"still-blockchain/util/uint128"
	"strconv"

	"github.com/still-project/go-randomstill"
	"github.com/zeebo/blake3"
)

type Uint128 = uint128.Uint128

type BlockHeader struct {
	Version     uint8           `json:"version"` // starts at 0
	Height      uint64          `json:"height"`
	Timestamp   uint64          `json:"timestamp"`
	Nonce       uint32          `json:"nonce"`
	NonceExtra  [16]byte        `json:"nonce_extra"`
	OtherChains []HashingID     `json:"other_chains"`
	Recipient   address.Address `json:"recipient"`   // recipient of block's coinbase reward
	Ancestors   Ancestors       `json:"prev_hash"`   // previous block hash
	SideBlocks  []Commitment    `json:"side_blocks"` // list of block previous side blocks (most recent block first)
}

func (b BlockHeader) PrevHash() util.Hash {
	return b.Ancestors[0]
}

type Block struct {
	BlockHeader `json:"header"`

	Difficulty     Uint128            `json:"diff"`            // block difficulty
	CumulativeDiff Uint128            `json:"cumulative_diff"` // block cumulative difficulty
	Transactions   []transaction.TXID `json:"transactions"`    // list of transaction hashes
}

func (b Block) String() string {
	hash := b.Hash()

	var x string

	commitment := b.Commitment()

	x += "Block " + hex.EncodeToString(hash[:]) + "\n"
	x += "Version: " + strconv.FormatUint(uint64(b.Version), 10) + "\n"
	x += "Height: " + strconv.FormatUint(uint64(b.Height), 10) + "\n"
	x += "Miner: " + b.Recipient.String() + "\n"
	x += "Reward: " + util.FormatCoin(b.Reward()) + "\n"
	x += "Timestamp: " + strconv.FormatUint(uint64(b.Timestamp), 10) + "\n"
	x += "Difficulty: " + b.Difficulty.String() + "\n"
	x += fmt.Sprintf("Cumulative diff: %.3fk\n", b.CumulativeDiff.Float64()/1000)
	x += "Nonce: " + strconv.FormatUint(uint64(b.Nonce), 10) + "\n"
	x += "Base hash: " + commitment.BaseHash.String() + "\n"
	hid := commitment.HashingID()
	x += "This chain hashing id: " + strconv.FormatUint(hid.NetworkID, 16) + " " +
		hex.EncodeToString(hid.Hash[:]) + "\n"
	x += "MiningBlob: " + commitment.MiningBlob().String() + "\n"
	x += "Other chains: " + strconv.FormatUint(uint64(len(b.OtherChains)), 10) + "\n"
	for _, v := range b.OtherChains {
		x += fmt.Sprintf(" - %x\n", v)
	}
	x += "Transactions: " + strconv.FormatUint(uint64(len(b.Transactions)), 10) + "\n"
	for _, v := range b.Transactions {
		x += fmt.Sprintf(" - %x\n", v)
	}
	x += "Side blocks: " + strconv.FormatUint(uint64(len(b.SideBlocks)), 10) + "\n"
	for _, v := range b.SideBlocks {
		x += fmt.Sprintf(" - %v\n", v)
	}

	return x
}

func (b *Block) SortOtherChains() {
	slices.SortFunc(b.OtherChains, func(a, b HashingID) int {
		if a.NetworkID < b.NetworkID {
			return -1
		} else if a.NetworkID > b.NetworkID {
			return 1
		}
		panic("possible duplicate value in OtherChains")
	})
}

func (b *Block) setMiningBlob(m MiningBlob) error {
	b.Timestamp = m.Timestamp
	b.Nonce = m.Nonce
	b.NonceExtra = m.NonceExtra
	b.OtherChains = make([]HashingID, 0)
	containsNetworkID := false
	var lastNetworkId uint64 = 0
	for _, v := range m.Chains {
		if v.NetworkID != config.NETWORK_ID {
			if v.NetworkID <= lastNetworkId {
				return fmt.Errorf("mining blob is not sorted correctly")
			}
			for _, oc := range b.OtherChains {
				if oc.Hash == v.Hash || oc.NetworkID == v.NetworkID {
					return fmt.Errorf("duplicate hashing id 0x%x %x", v.NetworkID, v.Hash)
				}
			}
			b.OtherChains = append(b.OtherChains, v)
			return nil
		} else {
			if containsNetworkID {
				return fmt.Errorf("mining blob has duplicate network id")
			}
			containsNetworkID = true
		}
	}
	if !containsNetworkID {
		return fmt.Errorf("mining blob does not contain current network id 0x%x", config.NETWORK_ID)
	}
	return nil
}

// Sets the block's OtherChains to sorted chains of MiningBlob.
func (b *Block) SetMiningBlob(m MiningBlob) error {
	if config.IS_MASTERCHAIN {
		panic("SetMiningBlob should never be called in masterchain")
	}

	return b.setMiningBlob(m)
}

func (b BlockHeader) Serialize() []byte {
	s := binary.NewSer(make([]byte, 75))

	s.AddUint8(b.Version)
	s.AddUvarint(b.Height)
	s.AddUvarint(b.Timestamp)
	s.AddUint32(b.Nonce)
	s.AddFixedByteArray(b.NonceExtra[:])
	s.AddFixedByteArray(b.Recipient[:])

	for _, v := range b.Ancestors {
		s.AddFixedByteArray(v[:])
	}

	s.AddUvarint(uint64(len(b.OtherChains)))
	for _, v := range b.OtherChains {
		s.AddUint64(v.NetworkID)
		s.AddFixedByteArray(v.Hash[:])
	}

	s.AddUvarint(uint64(len(b.SideBlocks)))
	for _, v := range b.SideBlocks {
		s.AddFixedByteArray(v.Serialize())
	}

	return s.Output()
}
func (b *BlockHeader) Deserialize(data []byte) ([]byte, error) {
	d := binary.NewDes(data)

	b.Version = d.ReadUint8()
	b.Height = d.ReadUvarint()
	b.Timestamp = d.ReadUvarint()
	b.Nonce = d.ReadUint32()
	b.NonceExtra = [16]byte(d.ReadFixedByteArray(16))
	b.Recipient = address.Address(d.ReadFixedByteArray(address.SIZE))

	for i := range b.Ancestors {
		b.Ancestors[i] = [32]byte(d.ReadFixedByteArray(32))
	}

	if d.Error() != nil {
		return nil, d.Error()
	}

	numChains := int(d.ReadUvarint())
	if numChains < 0 || numChains > config.MAX_MERGE_MINED_CHAINS-1 {
		return d.RemainingData(), fmt.Errorf("OtherChains exceed limit: %d", numChains)
	}
	b.OtherChains = make([]HashingID, numChains)
	// check that there are no duplicate chains
	for i := range b.OtherChains {
		if d.Error() != nil {
			return d.RemainingData(), d.Error()
		}
		b.OtherChains[i] = HashingID{
			NetworkID: d.ReadUint64(),
			Hash:      [32]byte(d.ReadFixedByteArray(32)),
		}
	}

	numSideBlocks := int(d.ReadUvarint())
	if numSideBlocks < 0 || numSideBlocks > config.MAX_SIDE_BLOCKS {
		return d.RemainingData(), fmt.Errorf("side blocks exceed limit: %d", numSideBlocks)
	}
	b.SideBlocks = make([]Commitment, numSideBlocks)
	for i := range b.SideBlocks {
		if d.Error() != nil {
			return d.RemainingData(), d.Error()
		}
		var err error
		d.Data, err = b.SideBlocks[i].Deserialize(d.RemainingData())
		if err != nil {
			return d.Data, err
		}
	}

	return d.RemainingData(), d.Error()
}

func (b Block) Serialize() []byte {
	s := binary.NewSer(make([]byte, 80))

	s.AddFixedByteArray(b.BlockHeader.Serialize())

	if b.Difficulty.IsZero() {
		return nil
	}

	// difficulty is encoded as a little-endian byte slice, with leading zero bytes removed
	diff := make([]byte, 16)
	b.Difficulty.PutBytes(diff)
	for len(diff) > 0 && diff[len(diff)-1] == 0 {
		diff = diff[:len(diff)-1]
	}
	s.AddByteSlice(diff)

	// cumulative difficulty is encoded the same way as difficulty
	diff = make([]byte, 16)
	b.CumulativeDiff.PutBytes(diff)
	for len(diff) > 0 && diff[len(diff)-1] == 0 {
		diff = diff[:len(diff)-1]
	}
	s.AddByteSlice(diff)

	// add transactions
	s.AddUvarint(uint64(len(b.Transactions)))
	for _, v := range b.Transactions {
		s.AddFixedByteArray(v[:])
	}

	return s.Output()
}
func (b *Block) Deserialize(data []byte) error {
	data, err := b.BlockHeader.Deserialize(data)
	if err != nil {
		return err
	}

	d := binary.NewDes(data)

	// read difficulty
	diff := make([]byte, 16)
	copy(diff, d.ReadByteSlice())
	b.Difficulty = Uint128{
		Hi: binary.LittleEndian.Uint64(diff[8:]),
		Lo: binary.LittleEndian.Uint64(diff[:8]),
	}
	// read cumulative difficulty
	diff = make([]byte, 16)
	copy(diff, d.ReadByteSlice())
	b.CumulativeDiff = Uint128{
		Hi: binary.LittleEndian.Uint64(diff[8:]),
		Lo: binary.LittleEndian.Uint64(diff[:8]),
	}

	if d.Error() != nil {
		return d.Error()
	}

	// read transactions
	numTx := d.ReadUvarint()
	if d.Error() != nil {
		return d.Error()
	}
	if numTx > config.MAX_TX_PER_BLOCK {
		return fmt.Errorf("block has too many transactions: %d, max: %d", numTx, config.MAX_TX_PER_BLOCK)
	}
	b.Transactions = make([]transaction.TXID, numTx)
	for i := uint64(0); i < numTx; i++ {
		txhash := [32]byte(d.ReadFixedByteArray(32))
		if d.Error() != nil {
			return d.Error()
		}
		b.Transactions[i] = txhash
	}

	return d.Error()
}

// deserializes the full block, which includes tx data. Used in P2P.
func (b *Block) DeserializeFull(data []byte) ([]*transaction.Transaction, error) {
	data, err := b.BlockHeader.Deserialize(data)
	if err != nil {
		return nil, err
	}

	d := binary.Des{
		Data: data,
	}

	// read difficulty
	diff := make([]byte, 16)
	copy(diff, d.ReadByteSlice())
	b.Difficulty = Uint128{
		Hi: binary.LittleEndian.Uint64(diff[8:]),
		Lo: binary.LittleEndian.Uint64(diff[:8]),
	}
	// read cumulative difficulty
	diff = make([]byte, 16)
	copy(diff, d.ReadByteSlice())
	b.CumulativeDiff = Uint128{
		Hi: binary.LittleEndian.Uint64(diff[8:]),
		Lo: binary.LittleEndian.Uint64(diff[:8]),
	}

	numTx := d.ReadUvarint()

	if d.Error() != nil {
		return nil, d.Error()
	}

	if numTx > config.MAX_TX_PER_BLOCK {
		return nil, fmt.Errorf("block has too many transactions: %d, max: %d", numTx, config.MAX_TX_PER_BLOCK)
	}

	txs := make([]*transaction.Transaction, numTx)
	b.Transactions = make([]transaction.TXID, numTx)
	for i := uint64(0); i < numTx; i++ {
		sl := d.ReadByteSlice()

		tx := transaction.Transaction{}
		err := tx.Deserialize(sl)
		if err != nil {
			return nil, err
		}

		txhash := tx.Hash()

		b.Transactions[i] = txhash
		txs[i] = &tx
	}

	return txs, d.Error()
}

func (b Block) Hash() util.Hash {
	return blake3.Sum256(b.Serialize()[:])
}

func (c Commitment) PowHash(seed randomstill.Seed) [16]byte {
	hash := randomstill.PowHash(seed, c.MiningBlob().Serialize())

	return [16]byte(hash[16:])
}
func (c Commitment) PowValue(seed randomstill.Seed) Uint128 {
	pow := c.PowHash(seed)
	upow := uint128.FromBytes(pow[:])
	return upow
}
func (b Block) ValidPowHash(hash [16]byte) bool {
	val := uint128.FromBytes(hash[:])

	return val.Cmp(uint128.Max.Div(b.Difficulty)) <= 0
}
func (c Commitment) ValidPowHash(seed randomstill.Seed, diff Uint128) bool {
	return c.PowValue(seed).Cmp(uint128.Max.Div(diff)) <= 0
}
func ValidPowValue(val Uint128, diff Uint128) bool {
	return val.Cmp(uint128.Max.Div(diff)) <= 0
}

// Prevalidate contains basic validity check, such as PoW hash and timestamp not in future
func (b Block) Prevalidate() error {
	// Generally, try insering the least expensive checks first, most expensive last

	if b.Version != 0 {
		return fmt.Errorf("unexpected block version %d", b.Version)
	}

	if b.Difficulty.IsZero() {
		return errors.New("difficulty is zero")
	}

	if b.Difficulty.Cmp64(config.MIN_DIFFICULTY) < 0 {
		return errors.New("difficulty is less than minimum")
	}

	if b.Timestamp > util.Time()+config.FUTURE_TIME_LIMIT*1000 {
		return errors.New("block is too much in the future")
	}

	// check that OtherChains are valid (no duplicates)
	for i, v := range b.OtherChains {
		if v.NetworkID == config.NETWORK_ID {
			return fmt.Errorf("other chain %x includes current network id", v.Hash)
		}
		for i2, v2 := range b.OtherChains {
			if i != i2 && (v.Hash == v2.Hash || v.NetworkID == v2.NetworkID) {
				return fmt.Errorf("duplicate OtherChain: %x %d; %x %d", v.Hash, v.NetworkID,
					v2.Hash, v2.NetworkID)
			}
		}
	}

	if !checkpoints.IsSecured(b.Height) {
		commitment := b.Commitment()
		mb := commitment.MiningBlob()
		seed := mb.GetSeed()
		powhash := commitment.PowHash(seed)
		if !b.ValidPowHash(powhash) {
			return fmt.Errorf("block %x with PoW %x does not meet difficulty", b.Hash(), powhash)
		}

		// prevalidate side blocks
		for _, side := range b.SideBlocks {
			/*if side.Height < b.Height-config.MINIDAG_ANCESTORS || side.Height >= b.Height {
				return fmt.Errorf("side block has invalid height %d, current block has height %d", side.Height, b.Height)
			}*/
			if GetSeedhashId(side.Timestamp) != GetSeedhashId(b.Timestamp) {
				return fmt.Errorf("side block has a different seedhash")
			}
			//
			// verify that side block's difficulty is at least 2/3 of current block difficulty
			if !side.ValidPowHash(seed, b.Difficulty.Mul64(2).Div64(3)) {
				return fmt.Errorf("commitment does not meet difficulty")
			}
		}
	} else {
		if checkpoints.IsCheckpoint(b.Height) {
			expectedHash := checkpoints.GetCheckpoint(b.Height)
			h := b.Hash()
			if h != expectedHash {
				return fmt.Errorf("block %x does not match checkpoint %x", h, expectedHash)
			}
		}
	}

	return nil
}
