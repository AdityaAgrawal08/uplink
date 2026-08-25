package crc64

import (
	"hash"
)

// NVMe CRC-64 Polynomial (Reflected representation)
const Poly = 0x9A6C9329AC4BC9B5

type Table [256]uint64

func MakeTable(poly uint64) *Table {
	t := new(Table)
	for i := 0; i < 256; i++ {
		crc := uint64(i)
		for j := 0; j < 8; j++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
		t[i] = crc
	}
	return t
}

var TableNVME = MakeTable(Poly)

type digest struct {
	crc uint64
	tab *Table
}

func New() hash.Hash64 {
	return &digest{crc: 0, tab: TableNVME}
}

func (d *digest) Size() int      { return 8 }
func (d *digest) BlockSize() int { return 1 }

func (d *digest) Reset() { d.crc = 0 }

func (d *digest) Write(p []byte) (n int, err error) {
	crc := d.crc
	for _, v := range p {
		crc = d.tab[byte(crc)^v] ^ (crc >> 8)
	}
	d.crc = crc
	return len(p), nil
}

func (d *digest) Sum64() uint64 {
	return d.crc
}

func (d *digest) Sum(in []byte) []byte {
	s := d.Sum64()
	return append(in,
		byte(s>>56),
		byte(s>>48),
		byte(s>>40),
		byte(s>>32),
		byte(s>>24),
		byte(s>>16),
		byte(s>>8),
		byte(s),
	)
}
