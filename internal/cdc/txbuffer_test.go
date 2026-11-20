package cdc

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func relFixture(id uint32, name string) *Relation {
	return &Relation{
		ID: id, Schema: "public", Name: name, ReplicaIdentity: 'd',
		Columns: []RelColumn{{Name: "id", Key: true, TypeOID: 23}, {Name: "doc", TypeOID: 25}},
	}
}

func insertChange(rel *Relation, id, doc string) Change {
	return Change{Op: OpInsert, Rel: rel, New: Tuple{{Kind: ValueText, Text: id}, {Kind: ValueText, Text: doc}}}
}

func collect(t *testing.T, b *ChangeBuffer) []Change {
	t.Helper()
	var out []Change
	if err := b.Iterate(func(c Change) error {
		out = append(out, c)
		return nil
	}); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	return out
}

func TestChangeBufferStaysInMemoryBelowTheLimit(t *testing.T) {
	dir := t.TempDir()
	b := NewChangeBuffer(1<<20, dir)
	defer b.Close()
	rel := relFixture(1, "t")
	for i := 0; i < 10; i++ {
		if err := b.Add(insertChange(rel, "1", "x")); err != nil {
			t.Fatal(err)
		}
	}
	if b.Spilled() || b.Len() != 10 || b.MemBytes() == 0 {
		t.Errorf("spilled=%v len=%d mem=%d", b.Spilled(), b.Len(), b.MemBytes())
	}
	if got := collect(t, b); len(got) != 10 {
		t.Errorf("got %d changes", len(got))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no spill file expected, found %d entries", len(entries))
	}
}

// A transaction above the memory limit is moved to disk and must come back identical and in order,
// including NULL, unchanged-TOAST, old tuples, truncates and every relation version.
func TestChangeBufferSpillsAndReadsBackInOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill") // must be created on demand
	b := NewChangeBuffer(2048, dir)
	rel1, rel2 := relFixture(1, "orders"), relFixture(2, "items")
	big := strings.Repeat("z", 700)

	var want []Change
	add := func(c Change) {
		t.Helper()
		if err := b.Add(c); err != nil {
			t.Fatal(err)
		}
		want = append(want, c)
	}
	add(insertChange(rel1, "1", big))
	add(insertChange(rel1, "2", big))
	add(Change{Op: OpUpdate, Rel: rel2, OldKind: OldKey,
		Old: Tuple{{Kind: ValueText, Text: "5"}, {Kind: ValueNull}},
		New: Tuple{{Kind: ValueText, Text: "6"}, {Kind: ValueToast}}})
	add(Change{Op: OpDelete, Rel: rel1, OldKind: OldFull,
		Old: Tuple{{Kind: ValueText, Text: "1"}, {Kind: ValueNull}}})
	// A new version of relation 1 appears later in the same transaction.
	rel1b := relFixture(1, "orders")
	rel1b.Columns = append(rel1b.Columns, RelColumn{Name: "extra", TypeOID: 25})
	add(Change{Op: OpInsert, Rel: rel1b, New: Tuple{{Kind: ValueText, Text: "9"}, {Kind: ValueNull}, {Kind: ValueText, Text: "e"}}})
	add(Change{Op: OpTruncate, Rels: []*Relation{rel1b, rel2}, TruncOpt: 3})

	if !b.Spilled() {
		t.Fatal("the buffer should have spilled")
	}
	if b.MemBytes() != 0 {
		t.Errorf("a spilled buffer holds no memory, got %d", b.MemBytes())
	}
	if b.Len() != len(want) {
		t.Errorf("Len = %d, want %d", b.Len(), len(want))
	}

	got := collect(t, b)
	if len(got) != len(want) {
		t.Fatalf("read back %d changes, want %d", len(got), len(want))
	}
	for i := range want {
		if !sameChange(got[i], want[i]) {
			t.Errorf("change %d differs:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	// The second iteration must work as well: a failed apply is retried from the start.
	if again := collect(t, b); len(again) != len(want) {
		t.Errorf("second pass returned %d changes", len(again))
	}

	files, _ := filepath.Glob(filepath.Join(dir, "tx-*.spill"))
	if len(files) != 1 {
		t.Fatalf("expected one spill file, found %v", files)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Errorf("Close must delete the spill file, stat err = %v", err)
	}
}

// sameChange compares two changes by content; relations are compared by value, not by pointer,
// because reading a spill file creates new relation objects.
func sameChange(a, b Change) bool {
	if a.Op != b.Op || a.OldKind != b.OldKind || a.TruncOpt != b.TruncOpt {
		return false
	}
	if !reflect.DeepEqual(a.New, b.New) || !reflect.DeepEqual(a.Old, b.Old) {
		return false
	}
	if (a.Rel == nil) != (b.Rel == nil) || (a.Rel != nil && !reflect.DeepEqual(*a.Rel, *b.Rel)) {
		return false
	}
	if len(a.Rels) != len(b.Rels) {
		return false
	}
	for i := range a.Rels {
		if !reflect.DeepEqual(*a.Rels[i], *b.Rels[i]) {
			return false
		}
	}
	return true
}

func TestTxDoneIsIdempotentAndReleases(t *testing.T) {
	released := 0
	tx := &Tx{Changes: NewChangeBuffer(1<<20, t.TempDir()), onDone: func() { released++ }}
	tx.Done()
	tx.Done()
	if released != 1 {
		t.Errorf("the budget must be released exactly once, got %d", released)
	}
}

func TestIterateStopsAtTheFirstError(t *testing.T) {
	b := NewChangeBuffer(1<<20, t.TempDir())
	defer b.Close()
	rel := relFixture(1, "t")
	for i := 0; i < 3; i++ {
		_ = b.Add(insertChange(rel, "1", "x"))
	}
	n := 0
	err := b.Iterate(func(Change) error {
		n++
		return os.ErrInvalid
	})
	if err != os.ErrInvalid || n != 1 {
		t.Errorf("err = %v, calls = %d", err, n)
	}
}
