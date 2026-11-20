// Package applier replays committed source transactions on s3 exactly once: the business statements
// and the update of applied_lsn run in one s3 transaction, so a crash leaves either both or neither.
package applier

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/shinession/gophant/internal/cdc"
	"github.com/shinession/gophant/internal/pg"
)

// Kind classifies a statement, which decides how its result is checked.
type Kind int

// The kinds of statement the builder produces.
const (
	KindInsert Kind = iota
	KindUpdate
	KindDelete
	KindTruncate
)

// Stmt is one SQL statement with its arguments. Every argument is text or nil: the values come from
// pgoutput in text format and PostgreSQL parses them for the type of the column.
type Stmt struct {
	SQL  string
	Args []any
	Kind Kind
	// Desc names the operation without row values, for error messages.
	Desc string
}

// ColumnMeta describes one column of the table on s3.
type ColumnMeta struct {
	// Generated columns are computed by s3 and never written.
	Generated bool
	// IdentityAlways is GENERATED ALWAYS AS IDENTITY: an INSERT needs OVERRIDING SYSTEM VALUE and an
	// UPDATE cannot assign it.
	IdentityAlways bool
	// TextCompare marks a type without a usable equality operator (json, xml, geometric types):
	// comparing the text form is the only way to locate a row by it.
	TextCompare bool
}

// TableMeta describes a table of s3 as far as the statement builder needs it.
type TableMeta struct {
	Columns map[string]ColumnMeta
	// HasPK tells whether the table has a primary key. Without one, rows are located by their full
	// content and a ctid guard makes sure that exactly one of several identical rows is changed.
	HasPK bool
	// Partitioned marks a partitioned root: ONLY must not be used, and ctid is only unique together
	// with tableoid.
	Partitioned bool
}

func (tm *TableMeta) column(rel *cdc.Relation, name string) (ColumnMeta, error) {
	cm, ok := tm.Columns[name]
	if !ok {
		return ColumnMeta{}, fmt.Errorf("schema drift: column %q of %s exists on s1 but not on s3", name, rel.FullName())
	}
	return cm, nil
}

// BuildInsert builds the INSERT of a new row. Generated columns are left out, and OVERRIDING SYSTEM
// VALUE is added when the row carries a value for an identity-always column.
func BuildInsert(rel *cdc.Relation, tm *TableMeta, t cdc.Tuple) (Stmt, error) {
	if len(t) != len(rel.Columns) {
		return Stmt{}, fmt.Errorf("insert into %s: the tuple has %d values for %d columns", rel.FullName(), len(t), len(rel.Columns))
	}
	var cols, placeholders []string
	var args []any
	override := false
	for i, c := range rel.Columns {
		cm, err := tm.column(rel, c.Name)
		if err != nil {
			return Stmt{}, err
		}
		if cm.Generated {
			continue
		}
		v := t[i]
		if v.Kind == cdc.ValueToast {
			return Stmt{}, fmt.Errorf("insert into %s: column %q is an unchanged TOAST value, which an insert cannot carry", rel.FullName(), c.Name)
		}
		args = append(args, argOf(v))
		cols = append(cols, pg.Ident(c.Name))
		placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		override = override || cm.IdentityAlways
	}

	target := pg.Qualified(rel.Schema, rel.Name)
	desc := "INSERT " + rel.FullName()
	if len(cols) == 0 {
		return Stmt{SQL: "INSERT INTO " + target + " DEFAULT VALUES", Kind: KindInsert, Desc: desc}, nil
	}
	sql := "INSERT INTO " + target + " (" + strings.Join(cols, ", ") + ")"
	if override {
		sql += " OVERRIDING SYSTEM VALUE"
	}
	sql += " VALUES (" + strings.Join(placeholders, ", ") + ")"
	return Stmt{SQL: sql, Args: args, Kind: KindInsert, Desc: desc}, nil
}

// BuildUpdate builds the UPDATE of a row. skip is true when nothing is left to assign.
//
// A column whose value arrived as an unchanged TOAST value is not part of the SET list: assigning
// anything to it would destroy the value that s3 already has (hard rule: UnchangedTOAST).
//
// The row is located by the replica identity columns. They are taken from the old tuple when one was
// sent (the key changed, or REPLICA IDENTITY FULL), otherwise from the new tuple, because then the
// key did not change.
func BuildUpdate(rel *cdc.Relation, tm *TableMeta, oldKind byte, oldT, newT cdc.Tuple) (st Stmt, skip bool, err error) {
	if len(newT) != len(rel.Columns) {
		return Stmt{}, false, fmt.Errorf("update of %s: the tuple has %d values for %d columns", rel.FullName(), len(newT), len(rel.Columns))
	}
	keys, err := keyColumns(rel)
	if err != nil {
		return Stmt{}, false, err
	}

	var sets []string
	var args []any
	for i, c := range rel.Columns {
		cm, err := tm.column(rel, c.Name)
		if err != nil {
			return Stmt{}, false, err
		}
		v := newT[i]
		switch {
		case cm.Generated, v.Kind == cdc.ValueToast:
			continue
		case cm.IdentityAlways:
			// An identity-always column cannot be assigned by UPDATE. A change is only possible through
			// "SET col = DEFAULT"; it is detected when the old value is known, and refused.
			if known, differs := oldValueDiffers(rel, i, oldKind, oldT, v); known && differs {
				return Stmt{}, false, fmt.Errorf("update of %s: the value of the identity column %q changed, which cannot be replayed", rel.FullName(), c.Name)
			}
			continue
		}
		args = append(args, argOf(v))
		sets = append(sets, pg.Ident(c.Name)+" = $"+strconv.Itoa(len(args)))
	}
	if len(sets) == 0 {
		return Stmt{}, true, nil
	}

	src := newT
	if oldT != nil {
		src = oldT
	}
	where, whereArgs, err := whereClause(rel, tm, keys, src, len(args))
	if err != nil {
		return Stmt{}, false, err
	}
	args = append(args, whereArgs...)

	target := pg.Qualified(rel.Schema, rel.Name)
	only := onlyKeyword(tm)
	sql := "UPDATE " + only + target + " SET " + strings.Join(sets, ", ") + " WHERE " + locate(rel, tm, target, where)
	return Stmt{SQL: sql, Args: args, Kind: KindUpdate, Desc: "UPDATE " + rel.FullName() + " by " + keyNames(rel, keys)}, false, nil
}

// BuildDelete builds the DELETE of a row, located by the old tuple.
func BuildDelete(rel *cdc.Relation, tm *TableMeta, oldT cdc.Tuple) (Stmt, error) {
	if oldT == nil {
		return Stmt{}, fmt.Errorf("delete from %s: the message has no old tuple", rel.FullName())
	}
	keys, err := keyColumns(rel)
	if err != nil {
		return Stmt{}, err
	}
	where, args, err := whereClause(rel, tm, keys, oldT, 0)
	if err != nil {
		return Stmt{}, err
	}
	target := pg.Qualified(rel.Schema, rel.Name)
	sql := "DELETE FROM " + onlyKeyword(tm) + target + " WHERE " + locate(rel, tm, target, where)
	return Stmt{SQL: sql, Args: args, Kind: KindDelete, Desc: "DELETE " + rel.FullName() + " by " + keyNames(rel, keys)}, nil
}

// BuildTruncate builds one TRUNCATE for all tables of the message. The tables are listed together
// so that foreign keys between them do not get in the way.
func BuildTruncate(rels []*cdc.Relation, option uint8) Stmt {
	names := make([]string, len(rels))
	descs := make([]string, len(rels))
	for i, r := range rels {
		names[i] = pg.Qualified(r.Schema, r.Name)
		descs[i] = r.FullName()
	}
	sql := "TRUNCATE " + strings.Join(names, ", ")
	if option&pglogrepl.TruncateOptionRestartIdentity != 0 {
		sql += " RESTART IDENTITY"
	}
	if option&pglogrepl.TruncateOptionCascade != 0 {
		sql += " CASCADE"
	}
	return Stmt{SQL: sql, Kind: KindTruncate, Desc: "TRUNCATE " + strings.Join(descs, ", ")}
}

// keyColumns returns the indexes of the replica identity columns of the relation.
func keyColumns(rel *cdc.Relation) ([]int, error) {
	var keys []int
	for i, c := range rel.Columns {
		if c.Key {
			keys = append(keys, i)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s has no replica identity columns: UPDATE and DELETE cannot be located (REPLICA IDENTITY %c)", rel.FullName(), rel.ReplicaIdentity)
	}
	return keys, nil
}

// whereClause builds the conditions that locate a row from the key columns of src. A NULL is
// matched with IS NULL, never with "= NULL". Argument numbers continue after argBase.
func whereClause(rel *cdc.Relation, tm *TableMeta, keys []int, src cdc.Tuple, argBase int) (conds []string, args []any, err error) {
	if len(src) != len(rel.Columns) {
		return nil, nil, fmt.Errorf("%s: the identity tuple has %d values for %d columns", rel.FullName(), len(src), len(rel.Columns))
	}
	for _, i := range keys {
		c := rel.Columns[i]
		cm, err := tm.column(rel, c.Name)
		if err != nil {
			return nil, nil, err
		}
		v := src[i]
		switch v.Kind {
		case cdc.ValueNull:
			conds = append(conds, pg.Ident(c.Name)+" IS NULL")
		case cdc.ValueText:
			args = append(args, v.Text)
			ph := "$" + strconv.Itoa(argBase+len(args))
			if cm.TextCompare {
				conds = append(conds, pg.Ident(c.Name)+"::text = "+ph)
			} else {
				conds = append(conds, pg.Ident(c.Name)+" = "+ph)
			}
		default:
			return nil, nil, fmt.Errorf("%s: identity column %q arrived as an unchanged TOAST value", rel.FullName(), c.Name)
		}
	}
	return conds, args, nil
}

// locate returns the WHERE condition for the located row. For a table without primary key whose
// identity is the full row it adds the ctid guard: identical rows are legal there, and the statement
// must change exactly one of them.
//
//	ctid = (SELECT ctid FROM ONLY t WHERE <identity> LIMIT 1)
//
// For a partitioned root ctid is only unique together with tableoid.
func locate(rel *cdc.Relation, tm *TableMeta, target string, conds []string) string {
	where := strings.Join(conds, " AND ")
	if rel.ReplicaIdentity != 'f' || tm.HasPK {
		return where
	}
	if tm.Partitioned {
		return "(tableoid, ctid) = (SELECT tableoid, ctid FROM " + target + " WHERE " + where + " LIMIT 1)"
	}
	return "ctid = (SELECT ctid FROM ONLY " + target + " WHERE " + where + " LIMIT 1)"
}

// onlyKeyword is "ONLY " for an ordinary table: the change messages name the physical table, so rows
// of inheritance children must not be touched. A partitioned root has no rows of its own.
func onlyKeyword(tm *TableMeta) string {
	if tm.Partitioned {
		return ""
	}
	return "ONLY "
}

// oldValueDiffers compares the new value of column i with the old one. known is false when the
// old value is not available: a key-only old tuple only carries the identity columns.
func oldValueDiffers(rel *cdc.Relation, i int, oldKind byte, oldT cdc.Tuple, v cdc.Value) (known, differs bool) {
	if oldT == nil || len(oldT) != len(rel.Columns) {
		return false, false
	}
	if oldKind != cdc.OldFull && !rel.Columns[i].Key {
		return false, false
	}
	return true, oldT[i] != v
}

func keyNames(rel *cdc.Relation, keys []int) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = rel.Columns[k].Name
	}
	return "(" + strings.Join(names, ", ") + ")"
}

func argOf(v cdc.Value) any {
	if v.Kind == cdc.ValueNull {
		return nil
	}
	return v.Text
}
