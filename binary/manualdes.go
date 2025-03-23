package binary

import (
	"encoding/binary"
	"errors"
	"math/big"
	"runtime"
	"strconv"
	"strings"
)

func NewDes(Data []byte) Des {
	return Des{
		Data: Data,
	}
}

type Des struct {
	Data []byte
	err  error
}

func (d Des) RemainingData() []byte {
	return d.Data
}

func (s *Des) ReadUint8() uint8 {
	if s.err != nil {
		return 0
	}
	if len(s.Data) < 1 {
		s.err = errors.New(getCaller() + " invalid length")
		return 0
	}
	b := s.Data[0]
	s.Data = s.Data[1:]
	return b
}
func (s *Des) ReadUint16() uint16 {
	if s.err != nil {
		return 0
	}
	if len(s.Data) < 2 {
		s.err = errors.New(getCaller() + " invalid length")
		return 0
	}
	b := s.Data[:2]
	s.Data = s.Data[2:]
	return binary.LittleEndian.Uint16(b)
}
func (s *Des) ReadUint32() uint32 {
	if s.err != nil {
		return 0
	}
	if len(s.Data) < 4 {
		s.err = errors.New(getCaller() + " invalid length")
		return 0
	}
	b := s.Data[:4]
	s.Data = s.Data[4:]
	return binary.LittleEndian.Uint32(b)
}
func (s *Des) ReadUint64() uint64 {
	if s.err != nil {
		return 0
	}
	if len(s.Data) < 8 {
		s.err = errors.New(getCaller() + " invalid length")
		return 0
	}
	b := s.Data[:8]
	s.Data = s.Data[8:]
	return binary.LittleEndian.Uint64(b)
}
func (s *Des) ReadUvarint() uint64 {
	if s.err != nil {
		return 0
	}
	if len(s.Data) < 1 {
		s.err = errors.New(getCaller() + " invalid length")
		return 0
	}
	d, x := binary.Uvarint(s.Data)
	if x < 0 {
		s.err = errors.New(getCaller() + " invalid uvarint")
		return 0
	}
	s.Data = s.Data[x:]
	return d
}

func (s *Des) ReadFixedByteArray(length int) []byte {
	if s.err != nil {
		return make([]byte, length)
	}
	if len(s.Data) < length {
		s.err = errors.New(getCaller() + " invalid length")
		return make([]byte, length)
	}
	b := s.Data[:length]
	s.Data = s.Data[length:]
	return b
}
func (s *Des) ReadByteSlice() []byte {
	if s.err != nil {
		return []byte{}
	}
	if len(s.Data) < 1 {
		s.err = errors.New(getCaller() + " invalid length")
		return []byte{}
	}
	length, read := binary.Uvarint(s.Data)
	if read < 0 {
		s.err = errors.New(getCaller() + " invalid uvarint length")
		return []byte{}
	}
	s.Data = s.Data[read:]
	if len(s.Data) < int(length) {
		s.err = errors.New(getCaller() + " invalid binary length")
		return []byte{}
	}

	b := s.Data[:length]
	s.Data = s.Data[length:]
	return b
}
func (s *Des) ReadString() string {
	return string(s.ReadByteSlice())
}

func (s *Des) ReadBool() bool {
	if s.err != nil {
		return false
	}
	if len(s.Data) < 1 {
		s.err = errors.New(getCaller() + " invalid length")
		return false
	}
	b := s.Data[0]
	s.Data = s.Data[1:]

	switch b {
	case 1:
		return true
	case 2:
		return false
	default:
		s.err = errors.New(getCaller() + " invalid boolean value")
		return false
	}
}
func (s *Des) ReadBigInt() *big.Int {
	return (&big.Int{}).SetBytes(s.ReadByteSlice())
}

func (s *Des) Error() error {
	return s.err
}

func getCaller() string {
	_, file, line, _ := runtime.Caller(2)
	fileSpl := strings.Split(file, "/")
	return fileSpl[len(fileSpl)-1] + ":" + strconv.FormatInt(int64(line), 10)
}
