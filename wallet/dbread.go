package wallet

import (
	"crypto/rand"
	"encoding/json"
	"still-blockchain/binary"
	"still-blockchain/bitcrypto"
)

func (w *Wallet) decodeDatabase(data, pass []byte) error {
	d := binary.Des{
		Data: data,
	}

	salt := d.ReadFixedByteArray(16)
	time := d.ReadUint32()
	mem := d.ReadUint32()

	if d.Error() != nil {
		return d.Error()
	}

	p := bitcrypto.KDF(pass, salt, time, mem)

	cip, err := bitcrypto.NewCipher(p)
	if err != nil {
		return err
	}

	dec, err := cip.Decrypt(d.Data)

	if err != nil {
		return err
	}

	return json.Unmarshal(dec, &w.dbInfo)
}

func saveDatabase(dbInfo dbInfo, pass []byte, time, mem uint32) ([]byte, error) {
	s := binary.Ser{}

	salt := genSalt()

	s.AddFixedByteArray(salt[:])
	s.AddUint32(time)
	s.AddUint32(mem)

	p := bitcrypto.KDF(pass, salt[:], time, mem)

	cip, err := bitcrypto.NewCipher(p)
	if err != nil {
		return nil, err
	}

	dbData, err := json.Marshal(dbInfo)
	if err != nil {
		return nil, err
	}

	enc, err := cip.Encrypt(dbData)
	if err != nil {
		return nil, err
	}

	return append(s.Output(), enc...), nil
}

func genSalt() [16]byte {
	b := make([]byte, 16)

	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}

	return [16]byte(b)
}
