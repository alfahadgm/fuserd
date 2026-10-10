package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

// pakMagic ends every UE4 pak footer (FPakInfo::PakFile_Magic).
const pakMagic = 0x5A6F12E1

// PakEntry is one file in a pak's index.
type PakEntry struct {
	Name             string // relative to the mount point
	Offset, Size     int64
	UncompressedSize int64
	Compression      uint32 // 0 = none
	Encrypted        bool
}

// PakIndex is what ReadPakIndex recovers from a pak: its version, mount point and file list.
type PakIndex struct {
	Version    uint32
	MountPoint string
	Entries    []PakEntry
}

// ReadPakIndex parses a UE4.24 pak (version 8, the shape community song paks and fuser_pak_create.py use) from r.
// It reads the footer, then the index it points at. An encrypted index is an error: community paks are never
// encrypted, and one that is can't be inspected.
func ReadPakIndex(r io.ReaderAt, size int64) (*PakIndex, error) {
	// The footer (FPakInfo) is EncryptionKeyGuid[16], bEncryptedIndex[1], Magic, Version, IndexOffset, IndexSize,
	// IndexHash[20], then for version 8 a table of compression method names (32 bytes each). Its exact length
	// depends on that table, so find the magic near the end of the file.
	tail := int64(512)
	if tail > size {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := r.ReadAt(buf, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	at := -1
	for i := len(buf) - 44; i >= 17; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) == pakMagic {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, errors.New("not a pak: no footer magic")
	}
	if buf[at-1] != 0 {
		return nil, errors.New("pak index is encrypted")
	}
	version := binary.LittleEndian.Uint32(buf[at+4:])
	indexOffset := int64(binary.LittleEndian.Uint64(buf[at+8:]))
	indexSize := int64(binary.LittleEndian.Uint64(buf[at+16:]))
	if version < 3 || version > 11 {
		return nil, fmt.Errorf("unsupported pak version %d", version)
	}
	if indexOffset < 0 || indexSize <= 0 || indexOffset+indexSize > size || indexSize > 64<<20 {
		return nil, errors.New("pak index out of range")
	}
	idx := make([]byte, indexSize)
	if _, err := r.ReadAt(idx, indexOffset); err != nil {
		return nil, err
	}
	d := &decoder{b: idx}
	p := &PakIndex{Version: version, MountPoint: d.fstring()}
	count := d.i32()
	if count < 0 || count > 1<<20 {
		return nil, errors.New("pak index entry count out of range")
	}
	for i := int32(0); i < count && d.err == nil; i++ {
		e := PakEntry{Name: d.fstring()}
		e.Offset, e.Size, e.UncompressedSize = d.i64(), d.i64(), d.i64()
		e.Compression = d.u32() // CompressionMethodIndex (version 8+), CompressionFlags before
		d.skip(20)              // SHA-1
		if e.Compression != 0 {
			blocks := d.u32()
			d.skip(int(blocks) * 16)
		}
		e.Encrypted = d.u8() != 0
		d.u32() // CompressionBlockSize
		p.Entries = append(p.Entries, e)
	}
	if d.err != nil {
		return nil, fmt.Errorf("pak index: %w", d.err)
	}
	return p, nil
}

// decoder reads little-endian UE4 archive values, remembering the first overrun.
type decoder struct {
	b   []byte
	pos int
	err error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || d.pos+n > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	b := d.b[d.pos : d.pos+n]
	d.pos += n
	return b
}

func (d *decoder) skip(n int) { d.take(n) }

func (d *decoder) u8() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *decoder) u32() uint32 {
	if b := d.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (d *decoder) i32() int32 { return int32(d.u32()) }

func (d *decoder) i64() int64 {
	if b := d.take(8); b != nil {
		return int64(binary.LittleEndian.Uint64(b))
	}
	return 0
}

// fstring reads an FString: an int32 length including the terminator, positive for ANSI, negative for UTF-16.
func (d *decoder) fstring() string {
	n := d.i32()
	switch {
	case n == 0:
		return ""
	case n > 0:
		b := d.take(int(n))
		if len(b) == 0 {
			return ""
		}
		return string(b[:len(b)-1])
	default:
		b := d.take(int(-n) * 2)
		if len(b) < 2 {
			return ""
		}
		u := make([]uint16, len(b)/2-1)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[i*2:])
		}
		return string(utf16.Decode(u))
	}
}
