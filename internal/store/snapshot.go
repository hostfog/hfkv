package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Snapshot file layout:
//
//	"HFKV" version
//	per database:  opDB  uvarint(index)
//	per key:       [opExpire varint(unix ms)] kind  string(key)  payload
//	opEOF  crc32 (little endian, of everything before it)
//
// Strings are uvarint(length) followed by the bytes.
const (
	magic          = "HFKV"
	version  byte  = 1
	opDB     byte  = 0xFE
	opExpire byte  = 0xFC
	opEOF    byte  = 0xFF
	maxCount int64 = 1 << 32
)

var ErrCorrupt = errors.New("snapshot is damaged")

// Snapshot encodes every database to w.
func (s *Store) Snapshot(w io.Writer) error {
	h := crc32.NewIEEE()
	bw := bufio.NewWriter(io.MultiWriter(w, h))
	e := encoder{w: bw}
	bw.WriteString(magic)
	bw.WriteByte(version)
	for i, db := range s.DBs {
		if len(db.keys) == 0 {
			continue
		}
		bw.WriteByte(opDB)
		e.uvarint(uint64(i))
		for k, ent := range db.keys {
			at, hasTTL := db.expires[k]
			if hasTTL && at <= s.clock {
				continue
			}
			if hasTTL {
				bw.WriteByte(opExpire)
				e.varint(at)
			}
			bw.WriteByte(byte(ent.v.Kind))
			e.str(k)
			e.value(ent.v)
		}
	}
	bw.WriteByte(opEOF)
	if err := bw.Flush(); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, h.Sum32())
}

// Restore replaces the contents of s with a snapshot.
func (s *Store) Restore(data []byte) error {
	if len(data) < len(magic)+2+4 || string(data[:len(magic)]) != magic {
		return fmt.Errorf("%w: not an hfkv snapshot", ErrCorrupt)
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	if body[len(magic)] != version {
		return fmt.Errorf("snapshot version %d is not supported", body[len(magic)])
	}
	d := decoder{r: bufio.NewReader(bytes.NewReader(body[len(magic)+1:]))}

	s.Flush()
	var db *DB
	for {
		op, err := d.r.ReadByte()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		switch op {
		case opEOF:
			s.Dirty = 0
			return nil
		case opDB:
			i := d.uvarint()
			if d.err != nil || i >= uint64(len(s.DBs)) {
				return fmt.Errorf("%w: database %d out of range", ErrCorrupt, i)
			}
			db = s.DBs[i]
			continue
		}
		if db == nil {
			return fmt.Errorf("%w: key before database", ErrCorrupt)
		}
		var at int64
		if op == opExpire {
			at = d.varint()
			if op, err = d.r.ReadByte(); err != nil {
				return fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		}
		if op > byte(KindZSet) {
			return fmt.Errorf("%w: unknown type %d", ErrCorrupt, op)
		}
		key := d.str()
		v := d.value(Kind(op))
		if d.err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, d.err)
		}
		if at > 0 && at <= s.clock {
			continue
		}
		db.Load(key, v, at)
	}
}

// SaveFile writes a snapshot to path atomically (temp file, fsync, rename).
func (s *Store) SaveFile(path string, snapshot []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hfkv-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(snapshot); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type encoder struct {
	w   *bufio.Writer
	buf [binary.MaxVarintLen64]byte
}

func (e *encoder) uvarint(n uint64) { e.w.Write(e.buf[:binary.PutUvarint(e.buf[:], n)]) }
func (e *encoder) varint(n int64)   { e.w.Write(e.buf[:binary.PutVarint(e.buf[:], n)]) }

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.w.WriteString(s)
}

func (e *encoder) value(v *Value) {
	switch v.Kind {
	case KindString:
		e.str(v.Str)
	case KindList:
		e.uvarint(uint64(v.List.Len()))
		for _, s := range v.List.Items() {
			e.str(s)
		}
	case KindHash:
		e.uvarint(uint64(len(v.Hash)))
		for f, s := range v.Hash {
			e.str(f)
			e.str(s)
		}
	case KindSet:
		e.uvarint(uint64(len(v.Set)))
		for m := range v.Set {
			e.str(m)
		}
	case KindZSet:
		e.uvarint(uint64(len(v.ZSet.sorted)))
		for _, it := range v.ZSet.sorted {
			e.str(it.Member)
			binary.LittleEndian.PutUint64(e.buf[:8], math.Float64bits(it.Score))
			e.w.Write(e.buf[:8])
		}
	}
}

type decoder struct {
	r   *bufio.Reader
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	n, err := binary.ReadUvarint(d.r)
	d.err = err
	return n
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	n, err := binary.ReadVarint(d.r)
	d.err = err
	return n
}

func (d *decoder) count() int {
	n := d.uvarint()
	if int64(n) > maxCount {
		d.err = errors.New("count too large")
		return 0
	}
	return int(n)
}

func (d *decoder) str() string {
	n := d.count()
	if d.err != nil {
		return ""
	}
	buf := make([]byte, n)
	_, d.err = io.ReadFull(d.r, buf)
	return string(buf)
}

func (d *decoder) value(k Kind) *Value {
	if k == KindString {
		return NewString(d.str())
	}
	v := newValue(k)
	n := d.count()
	for i := 0; i < n && d.err == nil; i++ {
		switch k {
		case KindList:
			v.List.back = append(v.List.back, d.str())
		case KindHash:
			f := d.str()
			v.Hash[f] = d.str()
		case KindSet:
			v.Set[d.str()] = struct{}{}
		case KindZSet:
			m := d.str()
			var b [8]byte
			if _, err := io.ReadFull(d.r, b[:]); err != nil {
				d.err = err
				break
			}
			v.ZAdd(m, math.Float64frombits(binary.LittleEndian.Uint64(b[:])))
		}
	}
	return v
}
