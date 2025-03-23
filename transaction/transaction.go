package transaction

import (
	"encoding/hex"
	"errors"
	"fmt"
	"still-blockchain/address"
	"still-blockchain/config"
	"still-blockchain/util"

	"still-blockchain/binary"
	"still-blockchain/bitcrypto"

	"github.com/zeebo/blake3"
)

type Transaction struct {
	Sender    bitcrypto.Pubkey    // sender's public key
	Recipient address.Address     // recipient's address
	Signature bitcrypto.Signature // transaction signature
	Nonce     uint64              // count of transactions sent by the sender, starting from 1
	Amount    uint64              // amount excludes the fee
	Fee       uint64              // fee of the transaction
	Subaddr   uint64              // subaddress id
}

type TXID [32]byte

func (t Transaction) Serialize() []byte {
	s := binary.NewSer(make([]byte, 120))

	s.AddFixedByteArray(t.Sender[:])
	s.AddFixedByteArray(t.Recipient[:])
	s.AddFixedByteArray(t.Signature[:])

	s.AddUvarint(t.Subaddr)
	s.AddUvarint(t.Nonce)
	s.AddUvarint(t.Amount)
	s.AddUvarint(t.Fee)

	return s.Output()
}
func (t *Transaction) Deserialize(data []byte) error {
	d := binary.Des{
		Data: data,
	}

	t.Sender = [bitcrypto.PUBKEY_SIZE]byte(d.ReadFixedByteArray(bitcrypto.PUBKEY_SIZE))
	t.Recipient = [address.SIZE]byte(d.ReadFixedByteArray(address.SIZE))
	t.Signature = [bitcrypto.SIGNATURE_SIZE]byte(d.ReadFixedByteArray(bitcrypto.SIGNATURE_SIZE))

	t.Subaddr = d.ReadUvarint()
	t.Nonce = d.ReadUvarint()
	t.Amount = d.ReadUvarint()
	t.Fee = d.ReadUvarint()

	return d.Error()
}

func (t Transaction) Hash() TXID {
	return blake3.Sum256(t.Serialize())
}

// The base overhad of all transactions. A transaction's VSize cannot be smaller than this.
const base_overhead = bitcrypto.PUBKEY_SIZE /*sender*/ + address.SIZE /*recipient*/ +
	bitcrypto.SIGNATURE_SIZE /*signature*/ + 1 /*timestamp*/ + 1 /*nonce*/ + 1 /*amount*/ +
	1 /*balance*/ + 1 /*fee*/ + 1 /*unlocks count*/ + 1 /*subaddr*/

func (t Transaction) GetVirtualSize() uint64 {
	return base_overhead
}

func (t Transaction) SignatureData() []byte {
	t.Signature = bitcrypto.Signature{}

	return t.Serialize()
}

func (t *Transaction) Sign(pk bitcrypto.Privkey) error {
	sig, err := bitcrypto.Sign(t.SignatureData(), pk)

	t.Signature = sig

	return err
}

// executes partial verification of transaction data, should be used before blockchain AddTransaction
func (t *Transaction) Prevalidate() error {
	// verify VSize
	vsize := t.GetVirtualSize()

	if vsize > config.MAX_TX_SIZE {
		return fmt.Errorf("invalid vsize: %d > MAX_TX_SIZE", vsize)
	}

	// verify that amount is not zero
	amt := t.Amount
	if amt == 0 {
		return fmt.Errorf("transaction amount cannot be zero")
	}

	// verify sender address
	senderAddr := address.FromPubKey(t.Sender)
	if senderAddr == address.INVALID_ADDRESS {
		return errors.New("invalid sender public key")
	}

	// verify that sender is not recipient
	if senderAddr == t.Recipient {
		return errors.New("sender and recipient must be different")
	}

	// verify that fee is higher than minimum fee level
	if t.Fee < config.FEE_PER_BYTE*vsize {
		return fmt.Errorf("invalid transaction fee: got %d, expected at least %d", t.Fee,
			config.FEE_PER_BYTE*vsize)
	}

	// verify signature
	sigValid := bitcrypto.VerifySignature(t.Sender, t.SignatureData(), t.Signature)
	if !sigValid {
		return fmt.Errorf("invalid signature")
	}

	// TODO: check if there is something else to prevalidate here

	return nil
}

func (t *Transaction) String() string {
	hash := t.Hash()
	o := "Transaction " + hex.EncodeToString(hash[:]) + "\n"

	o += " VSize: " + util.FormatUint(t.GetVirtualSize()) + "; physical size: " + util.FormatInt(len(t.Serialize())) + "\n"
	o += " Sender: " + address.FromPubKey(t.Sender).Integrated().String() + "\n"
	o += " Recipient: " + t.Recipient.Integrated().String() + "\n"

	o += " Signature: " + hex.EncodeToString(t.Signature[:]) + "\n"

	o += " Nonce: " + util.FormatUint(t.Nonce) + "\n"
	o += " Amount: " + util.FormatUint(t.Amount) + "\n"
	o += " Fee: " + util.FormatUint(t.Fee)

	return o
}
