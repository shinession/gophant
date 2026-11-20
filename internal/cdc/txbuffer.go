package cdc

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pglogrepl"
)

// Tx is one complete, committed source transaction. Its changes are held in memory; a transaction
// larger than the memory limit is spilled to a temporary file and read back while it is replayed.
type Tx struct {
	Xid        uint32
	CommitLSN  pglogrepl.LSN
	EndLSN     pglogrepl.LSN
	CommitTime time.Time
	Changes    *ChangeBuffer

	onDone func()
	logger *slog.Logger
	done   bool
}

// Done releases the memory budget and removes the spill file. It is safe to call more than once. A
// spill file that cannot be removed is logged: it stays on disk until someone deletes it.
func (t *Tx) Done() {
	if t.done {
		return
	}
	t.done = true
	if err := t.Changes.Close(); err != nil && t.logger != nil {
		t.logger.Warn("could not remove the spill file of a transaction", "xid", t.Xid, "error", err)
	}
	if t.onDone != nil {
		t.onDone()
	}
}

const (
	// changeOverhead and valueOverhead approximate the memory a change and a value cost besides their text.
	changeOverhead = 96
	valueOverhead  = 24

	recRelation byte = 'R'
	recChange   byte = 'C'

	// absentTuple marks a tuple that is not present in a change.
	absentTuple = 0xFFFF
)

// ChangeBuffer holds the changes of one transaction in commit order. Up to maxMem bytes stay in
// memory; the first change that crosses the limit moves everything to a file in dir, and all later
// changes are appended there, so the file always holds the complete ordered sequence.
type ChangeBuffer struct {
	maxMem int64
	dir    string

	mem      []Change
	memBytes int64
	count    int

	file    *os.File
	enc     *encoder
	written map[uint32]*Relation // relation version last written to the file, per relation ID
}

// NewChangeBuffer returns an empty buffer that spills into dir after maxMem bytes.
func NewChangeBuffer(maxMem int64, dir string) *ChangeBuffer {
	return &ChangeBuffer{maxMem: maxMem, dir: dir}
}

// Len returns the number of changes.
func (b *ChangeBuffer) Len() int { return b.count }

// Spilled reports whether the changes are on disk.
func (b *ChangeBuffer) Spilled() bool { return b.file != nil }

// MemBytes returns the approximate memory held by the buffer; 0 once it is spilled.
func (b *ChangeBuffer) MemBytes() int64 { return b.memBytes }

// Add appends a change.
func (b *ChangeBuffer) Add(c Change) error {
	b.count++
	if b.file != nil {
		return b.writeChange(c)
	}
	b.mem = append(b.mem, c)
	b.memBytes += changeSize(c)
	if b.memBytes <= b.maxMem {
		return nil
	}
	return b.spill()
}

// Iterate calls fn for every change in order and stops at the first error.
func (b *ChangeBuffer) Iterate(fn func(Change) error) error {
	if b.file == nil {
		for _, c := range b.mem {
			if err := fn(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := b.enc.flush(); err != nil {
		return fmt.Errorf("write the spill file: %w", err)
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind the spill file: %w", err)
	}
	r := bufio.NewReaderSize(b.file, 1<<20)
	rels := make(map[uint32]*Relation)
	for {
		kind, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the spill file: %w", err)
		}
		switch kind {
		case recRelation:
			rel, err := readRelation(r)
			if err != nil {
				return err
			}
			rels[rel.ID] = rel
		case recChange:
			c, err := readChange(r, rels)
			if err != nil {
				return err
			}
			if err := fn(c); err != nil {
				return err
			}
		default:
			return fmt.Errorf("corrupt spill file: unknown record type %d", kind)
		}
	}
}

// Close drops the in-memory changes and deletes the spill file.
func (b *ChangeBuffer) Close() error {
	b.mem, b.memBytes = nil, 0
	if b.file == nil {
		return nil
	}
	name := b.file.Name()
	closeErr := b.file.Close()
	b.file, b.enc = nil, nil
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the spill file: %w", err)
	}
	return closeErr
}

// spill moves the in-memory changes to a new temporary file.
func (b *ChangeBuffer) spill() error {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return fmt.Errorf("create the spill directory: %w", err)
	}
	f, err := os.CreateTemp(b.dir, "tx-*.spill")
	if err != nil {
		return fmt.Errorf("create the spill file: %w", err)
	}
	b.file = f
	b.enc = &encoder{w: bufio.NewWriterSize(f, 1<<20)}
	b.written = make(map[uint32]*Relation)
	for _, c := range b.mem {
		if err := b.writeChange(c); err != nil {
			return err
		}
	}
	b.mem, b.memBytes = nil, 0
	return nil
}

func changeSize(c Change) int64 {
	n := int64(changeOverhead)
	for _, t := range []Tuple{c.New, c.Old} {
		for _, v := range t {
			n += valueOverhead + int64(len(v.Text))
		}
	}
	return n
}

// The spill file is a sequence of records. A relation record carries a relation definition and is
// written before the first change that uses that version of the relation; a change record refers to
// relations by ID, so reading the file back needs no other state.
//
//	relation: 'R' id:u32 schema:str name:str identity:u8 ncols:u16 { key:u8 name:str typeoid:u32 }*
//	change:   'C' op:u8 ( 'T': opt:u8 nrels:u16 id:u32* | other: id:u32 ) oldkind:u8 old:tuple new:tuple
//	tuple:    n:u16 (0xFFFF = absent) { kind:u8 [ len:u32 bytes ] }*
//	str:      len:u32 bytes
func (b *ChangeBuffer) writeChange(c Change) error {
	rels := c.Rels
	if c.Rel != nil {
		rels = []*Relation{c.Rel}
	}
	e := b.enc
	for _, rel := range rels {
		if b.written[rel.ID] == rel {
			continue
		}
		e.relation(rel)
		b.written[rel.ID] = rel
	}

	e.u8(recChange)
	e.u8(byte(c.Op))
	if c.Op == OpTruncate {
		e.u8(c.TruncOpt)
		e.u16(uint16(len(c.Rels)))
		for _, rel := range c.Rels {
			e.u32(rel.ID)
		}
	} else {
		e.u32(c.Rel.ID)
	}
	e.u8(c.OldKind)
	e.tuple(c.Old)
	e.tuple(c.New)
	if e.err != nil {
		return fmt.Errorf("write the spill file: %w", e.err)
	}
	return nil
}

// encoder writes the record format. bufio.Writer keeps the first error, so it is checked once per change.
type encoder struct {
	w   *bufio.Writer
	err error
}

func (e *encoder) flush() error {
	if e.err != nil {
		return e.err
	}
	e.err = e.w.Flush()
	return e.err
}

func (e *encoder) u8(v byte) {
	if e.err == nil {
		e.err = e.w.WriteByte(v)
	}
}

func (e *encoder) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	e.bytes(b[:])
}

func (e *encoder) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	e.bytes(b[:])
}

func (e *encoder) bytes(p []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(p)
	}
}

func (e *encoder) str(s string) {
	e.u32(uint32(len(s)))
	if e.err == nil {
		_, e.err = e.w.WriteString(s)
	}
}

func (e *encoder) relation(rel *Relation) {
	e.u8(recRelation)
	e.u32(rel.ID)
	e.str(rel.Schema)
	e.str(rel.Name)
	e.u8(rel.ReplicaIdentity)
	e.u16(uint16(len(rel.Columns)))
	for _, c := range rel.Columns {
		if c.Key {
			e.u8(1)
		} else {
			e.u8(0)
		}
		e.str(c.Name)
		e.u32(c.TypeOID)
	}
}

func (e *encoder) tuple(t Tuple) {
	if t == nil {
		e.u16(absentTuple)
		return
	}
	e.u16(uint16(len(t)))
	for _, v := range t {
		e.u8(v.Kind)
		if v.Kind == ValueText {
			e.str(v.Text)
		}
	}
}

func readRelation(r *bufio.Reader) (*Relation, error) {
	var rel Relation
	var err error
	if rel.ID, err = getU32(r); err != nil {
		return nil, spillErr(err)
	}
	if rel.Schema, err = getStr(r); err != nil {
		return nil, spillErr(err)
	}
	if rel.Name, err = getStr(r); err != nil {
		return nil, spillErr(err)
	}
	if rel.ReplicaIdentity, err = r.ReadByte(); err != nil {
		return nil, spillErr(err)
	}
	n, err := getU16(r)
	if err != nil {
		return nil, spillErr(err)
	}
	rel.Columns = make([]RelColumn, n)
	for i := range rel.Columns {
		key, err := r.ReadByte()
		if err != nil {
			return nil, spillErr(err)
		}
		rel.Columns[i].Key = key == 1
		if rel.Columns[i].Name, err = getStr(r); err != nil {
			return nil, spillErr(err)
		}
		if rel.Columns[i].TypeOID, err = getU32(r); err != nil {
			return nil, spillErr(err)
		}
	}
	return &rel, nil
}

func readChange(r *bufio.Reader, rels map[uint32]*Relation) (Change, error) {
	var c Change
	op, err := r.ReadByte()
	if err != nil {
		return c, spillErr(err)
	}
	c.Op = Op(op)
	lookup := func(id uint32) (*Relation, error) {
		rel, ok := rels[id]
		if !ok {
			return nil, fmt.Errorf("corrupt spill file: relation %d was not defined before its first use", id)
		}
		return rel, nil
	}
	if c.Op == OpTruncate {
		if c.TruncOpt, err = r.ReadByte(); err != nil {
			return c, spillErr(err)
		}
		n, err := getU16(r)
		if err != nil {
			return c, spillErr(err)
		}
		for range n {
			id, err := getU32(r)
			if err != nil {
				return c, spillErr(err)
			}
			rel, err := lookup(id)
			if err != nil {
				return c, err
			}
			c.Rels = append(c.Rels, rel)
		}
	} else {
		id, err := getU32(r)
		if err != nil {
			return c, spillErr(err)
		}
		if c.Rel, err = lookup(id); err != nil {
			return c, err
		}
	}
	if c.OldKind, err = r.ReadByte(); err != nil {
		return c, spillErr(err)
	}
	if c.Old, err = readTuple(r); err != nil {
		return c, err
	}
	if c.New, err = readTuple(r); err != nil {
		return c, err
	}
	return c, nil
}

func readTuple(r *bufio.Reader) (Tuple, error) {
	n, err := getU16(r)
	if err != nil {
		return nil, spillErr(err)
	}
	if n == absentTuple {
		return nil, nil
	}
	t := make(Tuple, n)
	for i := range t {
		kind, err := r.ReadByte()
		if err != nil {
			return nil, spillErr(err)
		}
		t[i].Kind = kind
		if kind == ValueText {
			if t[i].Text, err = getStr(r); err != nil {
				return nil, spillErr(err)
			}
		}
	}
	return t, nil
}

func spillErr(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("read the spill file: %w", err)
}

func getU16(r *bufio.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

func getU32(r *bufio.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func getStr(r *bufio.Reader) (string, error) {
	n, err := getU32(r)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
