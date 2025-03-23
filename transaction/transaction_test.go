package transaction_test

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"still-blockchain/address"
	"still-blockchain/bitcrypto"
	"still-blockchain/config"
	"still-blockchain/transaction"
	"testing"

	"github.com/zeebo/blake3"
)

func TestTransaction(t *testing.T) {
	privk := address.GenerateKeypair(blake3.Sum256([]byte("test")))
	pubk := privk.Public()

	if hex.EncodeToString(pubk[:]) != "87560320f9cd73a12ef35c886bcde72049d8e4d83ea3b32586270bc7d8e8e422" {
		t.Errorf("invalid public key %x", privk.Public())
	}

	recipient := address.Address{}
	rand.Read(recipient[:])

	tx := transaction.Transaction{
		Sender:    privk.Public(),
		Recipient: recipient,
		Signature: bitcrypto.Signature{},
		Nonce:     1,
		Amount:    config.COIN,
		Fee:       0,
	}
	tx.Fee = tx.GetVirtualSize() * config.FEE_PER_BYTE

	tx.Sign(privk)

	tx.Serialize()

	ser := tx.Serialize()

	t.Logf("transaction size: %d, data: %x", len(ser), ser)

	tx2 := transaction.Transaction{}
	err := tx2.Deserialize(ser)
	if err != nil {
		t.Error(err)
	}

	ser2 := tx2.Serialize()

	t.Logf("transaction size: %d, data: %x", len(ser2), ser2)

	if !slices.Equal(ser, ser2) {
		t.Error("second serialized transaction differs from original")
	}

	err = tx.Prevalidate()

	if err != nil {
		t.Error("transaction verification failed:", err)
	}

	t.Log(tx.String())
}
