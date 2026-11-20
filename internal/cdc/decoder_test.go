package cdc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/shinession/gophant/internal/config"
)

// The helpers below build pgoutput messages the way the server does, so the decoder is tested against
// the wire format and not against its own idea of it.

func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }
func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func cstr(s string) []byte { return append([]byte(s), 0) }

type col struct {
	flags   byte
	name    string
	typeOID uint32
}

func relationMsg(id uint32, schema, name string, identity byte, cols ...col) []byte {
	var b bytes.Buffer
	b.WriteByte('R')
	b.Write(be32(id))
	b.Write(cstr(schema))
	b.Write(cstr(name))
	b.WriteByte(identity)
	b.Write(be16(uint16(len(cols))))
	for _, c := range cols {
		b.WriteByte(c.flags)
		b.Write(cstr(c.name))
		b.Write(be32(c.typeOID))
		b.Write(be32(0xFFFFFFFF)) // type modifier -1
	}
	return b.Bytes()
}

// tupleData encodes values: nil = NULL, "\x00" = unchanged TOAST, anything else text.
func tupleData(values ...*string) []byte {
	var b bytes.Buffer
	b.Write(be16(uint16(len(values))))
	for _, v := range values {
		switch {
		case v == nil:
			b.WriteByte('n')
		case *v == "\x00":
			b.WriteByte('u')
		default:
			b.WriteByte('t')
			b.Write(be32(uint32(len(*v))))
			b.WriteString(*v)
		}
	}
	return b.Bytes()
}

func s(v string) *string { return &v }

func insertMsg(relID uint32, values ...*string) []byte {
	return append(append([]byte{'I'}, append(be32(relID), 'N')...), tupleData(values...)...)
}

func updateMsg(relID uint32, oldKind byte, old []*string, newer []*string) []byte {
	b := append([]byte{'U'}, be32(relID)...)
	if oldKind != 0 {
		b = append(b, oldKind)
		b = append(b, tupleData(old...)...)
	}
	b = append(b, 'N')
	return append(b, tupleData(newer...)...)
}

func deleteMsg(relID uint32, oldKind byte, old ...*string) []byte {
	b := append([]byte{'D'}, be32(relID)...)
	b = append(b, oldKind)
	return append(b, tupleData(old...)...)
}

func newTestDecoder(t *testing.T, yaml string) *Decoder {
	t.Helper()
	cfg, err := config.Parse([]byte("source:\n  dsn: a\ntarget:\n  dsn: b\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewDecoder(&cfg.Filter)
}

func TestDecodeBeginAndCommit(t *testing.T) {
	d := newTestDecoder(t, "")

	begin := append(append([]byte{'B'}, be64(0x1000)...), append(be64(0), be32(742)...)...)
	m, err := d.Decode(begin)
	if err != nil || m.Kind != MsgBegin || m.Begin.FinalLSN != 0x1000 || m.Begin.Xid != 742 {
		t.Fatalf("begin: %+v %v", m, err)
	}

	commit := append([]byte{'C', 0}, append(be64(0x1000), append(be64(0x1040), be64(0)...)...)...)
	m, err = d.Decode(commit)
	if err != nil || m.Kind != MsgCommit || m.Commit.CommitLSN != 0x1000 || m.Commit.EndLSN != 0x1040 {
		t.Fatalf("commit: %+v %v", m, err)
	}
}

func TestDecodeRelationAndInsert(t *testing.T) {
	d := newTestDecoder(t, "")
	if m, err := d.Decode(relationMsg(16400, "public", "customers", 'd', col{1, "id", 23}, col{0, "name", 25})); err != nil || m.Kind != MsgIgnored {
		t.Fatalf("a relation message is bookkeeping only: %+v %v", m, err)
	}

	m, err := d.Decode(insertMsg(16400, s("7"), nil))
	if err != nil || m.Kind != MsgChange || m.Change.Op != OpInsert {
		t.Fatalf("insert: %+v %v", m, err)
	}
	rel := m.Change.Rel
	if rel.FullName() != "public.customers" || rel.ReplicaIdentity != 'd' || len(rel.Columns) != 2 {
		t.Errorf("relation = %+v", rel)
	}
	if !rel.Columns[0].Key || rel.Columns[1].Key {
		t.Errorf("only the first column is part of the key: %+v", rel.Columns)
	}
	if got := m.Change.New; got[0] != (Value{Kind: ValueText, Text: "7"}) || got[1].Kind != ValueNull {
		t.Errorf("tuple = %+v", got)
	}
}

func TestDecodeUpdateKeepsUnchangedToastMarker(t *testing.T) {
	d := newTestDecoder(t, "")
	_, _ = d.Decode(relationMsg(1, "public", "blobs", 'd', col{1, "id", 23}, col{0, "doc", 25}, col{0, "meta", 23}))

	m, err := d.Decode(updateMsg(1, 0, nil, []*string{s("1"), s("\x00"), s("5")}))
	if err != nil || m.Kind != MsgChange || m.Change.Op != OpUpdate {
		t.Fatalf("update: %+v %v", m, err)
	}
	if m.Change.New[1].Kind != ValueToast {
		t.Errorf("an unchanged TOAST column must keep its marker, got %+v", m.Change.New[1])
	}
	if m.Change.Old != nil || m.Change.OldKind != OldNone {
		t.Errorf("no old tuple expected: %+v", m.Change)
	}
}

func TestDecodeUpdateWithKeyAndFullOldTuples(t *testing.T) {
	d := newTestDecoder(t, "")
	_, _ = d.Decode(relationMsg(1, "public", "t", 'd', col{1, "id", 23}, col{0, "v", 25}))

	m, err := d.Decode(updateMsg(1, 'K', []*string{s("1"), nil}, []*string{s("2"), s("x")}))
	if err != nil || m.Change.OldKind != OldKey || m.Change.Old[0].Text != "1" || m.Change.New[0].Text != "2" {
		t.Fatalf("key update: %+v %v", m, err)
	}

	m, err = d.Decode(updateMsg(1, 'O', []*string{s("1"), s("old")}, []*string{s("1"), s("new")}))
	if err != nil || m.Change.OldKind != OldFull || m.Change.Old[1].Text != "old" {
		t.Fatalf("full update: %+v %v", m, err)
	}
}

func TestDecodeDeleteAndTruncate(t *testing.T) {
	d := newTestDecoder(t, "")
	_, _ = d.Decode(relationMsg(1, "public", "a", 'd', col{1, "id", 23}))
	_, _ = d.Decode(relationMsg(2, "public", "b", 'd', col{1, "id", 23}))

	m, err := d.Decode(deleteMsg(1, 'K', s("9")))
	if err != nil || m.Change.Op != OpDelete || m.Change.OldKind != OldKey || m.Change.Old[0].Text != "9" {
		t.Fatalf("delete: %+v %v", m, err)
	}

	trunc := append([]byte{'T'}, be32(2)...)
	trunc = append(trunc, 3) // cascade + restart identity
	trunc = append(trunc, be32(1)...)
	trunc = append(trunc, be32(2)...)
	m, err = d.Decode(trunc)
	if err != nil || m.Change.Op != OpTruncate || len(m.Change.Rels) != 2 || m.Change.TruncOpt != 3 {
		t.Fatalf("truncate: %+v %v", m, err)
	}
	if m.Change.TruncOpt&pglogrepl.TruncateOptionCascade == 0 {
		t.Error("the cascade option must survive decoding")
	}
}

func TestDecoderDropsTablesOutsideTheScope(t *testing.T) {
	d := newTestDecoder(t, "filter:\n  include_schemas: [public]\n  exclude_tables: [\"public.audit\"]\n")
	_, _ = d.Decode(relationMsg(1, "public", "orders", 'd', col{1, "id", 23}))
	_, _ = d.Decode(relationMsg(2, "public", "audit", 'd', col{1, "id", 23}))
	_, _ = d.Decode(relationMsg(3, "other", "x", 'd', col{1, "id", 23}))

	if m, _ := d.Decode(insertMsg(1, s("1"))); m.Kind != MsgChange {
		t.Error("public.orders is in scope")
	}
	for _, id := range []uint32{2, 3} {
		m, err := d.Decode(insertMsg(id, s("1")))
		if err != nil || m.Kind != MsgIgnored {
			t.Errorf("relation %d is out of scope and must be ignored, got %+v %v", id, m, err)
		}
	}

	// A truncate that only touches excluded tables disappears; a mixed one keeps the tables in scope.
	allOut := append(append([]byte{'T'}, be32(2)...), 0)
	allOut = append(append(allOut, be32(2)...), be32(3)...)
	if m, err := d.Decode(allOut); err != nil || m.Kind != MsgIgnored {
		t.Errorf("a truncate of excluded tables must be ignored: %+v %v", m, err)
	}
	mixed := append(append([]byte{'T'}, be32(2)...), 0)
	mixed = append(append(mixed, be32(1)...), be32(2)...)
	if m, err := d.Decode(mixed); err != nil || len(m.Change.Rels) != 1 || m.Change.Rels[0].Name != "orders" {
		t.Errorf("a mixed truncate keeps only the tables in scope: %+v %v", m, err)
	}
}

func TestDecodeErrors(t *testing.T) {
	d := newTestDecoder(t, "")
	if _, err := d.Decode(insertMsg(99, s("1"))); err == nil || !strings.Contains(err.Error(), "never announced") {
		t.Errorf("a change for an unknown relation must fail, got %v", err)
	}
	_, _ = d.Decode(relationMsg(1, "public", "t", 'd', col{1, "id", 23}))
	if _, err := d.Decode(insertMsg(1, s("1"), s("2"))); err == nil || !strings.Contains(err.Error(), "columns") {
		t.Errorf("a tuple with the wrong number of columns must fail, got %v", err)
	}
	if _, err := d.Decode(nil); err == nil {
		t.Error("an empty message must fail")
	}
	// A streaming message (protocol v2) is not expected with proto_version 1.
	if _, err := d.Decode([]byte{'S', 0, 0, 0, 1, 1}); err == nil {
		t.Error("an unexpected message type must fail")
	}
}

func TestDecodeIgnoresOriginTypeAndLogicalMessages(t *testing.T) {
	d := newTestDecoder(t, "")
	typ := append(append([]byte{'Y'}, be32(1234)...), append(cstr("public"), cstr("mood")...)...)
	if m, err := d.Decode(typ); err != nil || m.Kind != MsgIgnored {
		t.Errorf("type message: %+v %v", m, err)
	}
	origin := append(append([]byte{'O'}, be64(5)...), cstr("origin")...)
	if m, err := d.Decode(origin); err != nil || m.Kind != MsgIgnored {
		t.Errorf("origin message: %+v %v", m, err)
	}
}

func TestProgressOnlyMovesForward(t *testing.T) {
	var p Progress
	p.AdvanceFlushed(100)
	p.AdvanceFlushed(50)
	if p.Flushed() != 100 {
		t.Errorf("Flushed went backwards: %v", p.Flushed())
	}
	p.AdvanceWritten(7)
	p.AdvanceWritten(9)
	p.AdvanceWritten(8)
	if p.Written() != 9 {
		t.Errorf("Written = %v", p.Written())
	}
	p.SetFast(true)
	if !p.Fast() || p.InFlight() != 0 {
		t.Error("fast flag or in-flight counter wrong")
	}
}
