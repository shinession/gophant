// Package cdc consumes the logical replication stream of s1 (pgoutput, protocol version 1) and turns
// it into complete transactions: it decodes the messages, keeps the relation map, buffers the changes
// of a transaction (spilling very large ones to disk) and reports the position back to s1.
package cdc

import (
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/shinession/gophant/internal/config"
)

// Value kinds of a column in a tuple.
const (
	ValueNull  byte = 'n' // SQL NULL
	ValueText  byte = 't' // a value in text format
	ValueToast byte = 'u' // an unchanged TOASTed value: the server did not send it
)

// Value is one column of a tuple.
type Value struct {
	Kind byte
	Text string // set when Kind is ValueText
}

// Tuple holds one value per column of the relation message that announced the table.
type Tuple []Value

// RelColumn is one column of a relation as announced by s1. Generated columns are not announced.
type RelColumn struct {
	Name string
	// Key marks a column of the replica identity (the primary key, the replica identity index, or
	// every column for REPLICA IDENTITY FULL).
	Key     bool
	TypeOID uint32
}

// Relation is a table as announced by a pgoutput Relation message. With
// publish_via_partition_root the name is the one of the partitioned root table.
type Relation struct {
	ID     uint32
	Schema string
	Name   string
	// ReplicaIdentity is 'd' (default), 'n' (nothing), 'f' (full) or 'i' (index).
	ReplicaIdentity byte
	Columns         []RelColumn
}

// FullName returns schema.name.
func (r *Relation) FullName() string { return r.Schema + "." + r.Name }

// Op is the kind of a change.
type Op byte

// The kinds of change inside a transaction.
const (
	OpInsert   Op = 'I'
	OpUpdate   Op = 'U'
	OpDelete   Op = 'D'
	OpTruncate Op = 'T'
)

// Kinds of the old tuple of an UPDATE or DELETE.
const (
	OldNone byte = 0
	// OldKey: the old tuple holds only the replica identity columns. For an UPDATE it is present only
	// when one of them changed.
	OldKey byte = 'K'
	// OldFull: the old tuple holds the whole old row (REPLICA IDENTITY FULL).
	OldFull byte = 'O'
)

// Change is one row change, or one TRUNCATE, of a transaction.
type Change struct {
	Op Op
	// Rel is the table of an insert, update or delete.
	Rel *Relation
	// Rels are the tables of a truncate and TruncOpt its options (pglogrepl.TruncateOption*).
	Rels     []*Relation
	TruncOpt uint8

	New     Tuple
	Old     Tuple
	OldKind byte
}

// MsgKind classifies a decoded message.
type MsgKind int

// The messages that matter to the transaction assembler. Everything else is MsgIgnored.
const (
	MsgIgnored MsgKind = iota
	MsgBegin
	MsgCommit
	MsgChange
)

// BeginInfo is the content of a Begin message.
type BeginInfo struct {
	FinalLSN   pglogrepl.LSN
	Xid        uint32
	CommitTime time.Time
}

// CommitInfo is the content of a Commit message.
type CommitInfo struct {
	// CommitLSN is where the commit record starts; EndLSN is where it ends. Everything that
	// committed before EndLSN is applied once a transaction with this EndLSN is.
	CommitLSN  pglogrepl.LSN
	EndLSN     pglogrepl.LSN
	CommitTime time.Time
}

// Msg is a decoded replication message.
type Msg struct {
	Kind   MsgKind
	Begin  BeginInfo
	Commit CommitInfo
	Change Change
}

// Decoder decodes pgoutput messages and keeps the relation map that gives meaning to the relation
// IDs of later messages. Changes of tables outside the migration scope are dropped here, so they
// never take memory or disk space.
type Decoder struct {
	relations map[uint32]*Relation
	filter    *config.Filter
}

// NewDecoder returns a decoder that keeps only the tables the filter selects.
func NewDecoder(filter *config.Filter) *Decoder {
	return &Decoder{relations: make(map[uint32]*Relation), filter: filter}
}

// Decode decodes one logical replication message (the payload of an XLogData message).
func (d *Decoder) Decode(data []byte) (Msg, error) {
	if len(data) == 0 {
		return Msg{}, fmt.Errorf("empty replication message")
	}
	parsed, err := pglogrepl.Parse(data)
	if err != nil {
		return Msg{}, fmt.Errorf("decode %q message: %w", string(data[0]), err)
	}

	switch m := parsed.(type) {
	case *pglogrepl.RelationMessage:
		d.relations[m.RelationID] = relationFrom(m)
		return Msg{}, nil
	case *pglogrepl.BeginMessage:
		return Msg{Kind: MsgBegin, Begin: BeginInfo{FinalLSN: m.FinalLSN, Xid: m.Xid, CommitTime: m.CommitTime}}, nil
	case *pglogrepl.CommitMessage:
		return Msg{Kind: MsgCommit, Commit: CommitInfo{CommitLSN: m.CommitLSN, EndLSN: m.TransactionEndLSN, CommitTime: m.CommitTime}}, nil
	case *pglogrepl.InsertMessage:
		rel, err := d.relation(m.RelationID, "insert")
		if err != nil || !d.inScope(rel) {
			return Msg{}, err
		}
		tuple, err := tupleFrom(rel, m.Tuple)
		if err != nil {
			return Msg{}, err
		}
		return Msg{Kind: MsgChange, Change: Change{Op: OpInsert, Rel: rel, New: tuple}}, nil
	case *pglogrepl.UpdateMessage:
		rel, err := d.relation(m.RelationID, "update")
		if err != nil || !d.inScope(rel) {
			return Msg{}, err
		}
		c := Change{Op: OpUpdate, Rel: rel, OldKind: m.OldTupleType}
		if c.New, err = tupleFrom(rel, m.NewTuple); err != nil {
			return Msg{}, err
		}
		if m.OldTuple != nil {
			if c.Old, err = tupleFrom(rel, m.OldTuple); err != nil {
				return Msg{}, err
			}
		}
		return Msg{Kind: MsgChange, Change: c}, nil
	case *pglogrepl.DeleteMessage:
		rel, err := d.relation(m.RelationID, "delete")
		if err != nil || !d.inScope(rel) {
			return Msg{}, err
		}
		c := Change{Op: OpDelete, Rel: rel, OldKind: m.OldTupleType}
		if c.Old, err = tupleFrom(rel, m.OldTuple); err != nil {
			return Msg{}, err
		}
		return Msg{Kind: MsgChange, Change: c}, nil
	case *pglogrepl.TruncateMessage:
		c := Change{Op: OpTruncate, TruncOpt: m.Option}
		for _, id := range m.RelationIDs {
			rel, err := d.relation(id, "truncate")
			if err != nil {
				return Msg{}, err
			}
			if d.inScope(rel) {
				c.Rels = append(c.Rels, rel)
			}
		}
		if len(c.Rels) == 0 {
			return Msg{}, nil
		}
		return Msg{Kind: MsgChange, Change: c}, nil
	case *pglogrepl.TypeMessage, *pglogrepl.OriginMessage, *pglogrepl.LogicalDecodingMessage:
		return Msg{}, nil
	default:
		return Msg{}, fmt.Errorf("unexpected replication message %s: streaming of in-progress transactions is not enabled", parsed.Type())
	}
}

func (d *Decoder) relation(id uint32, what string) (*Relation, error) {
	rel, ok := d.relations[id]
	if !ok {
		return nil, fmt.Errorf("%s for relation %d that was never announced", what, id)
	}
	return rel, nil
}

func (d *Decoder) inScope(rel *Relation) bool { return d.filter.Match(rel.Schema, rel.Name) }

func relationFrom(m *pglogrepl.RelationMessage) *Relation {
	rel := &Relation{
		ID:              m.RelationID,
		Schema:          m.Namespace,
		Name:            m.RelationName,
		ReplicaIdentity: m.ReplicaIdentity,
		Columns:         make([]RelColumn, len(m.Columns)),
	}
	for i, c := range m.Columns {
		rel.Columns[i] = RelColumn{Name: c.Name, Key: c.Flags&1 != 0, TypeOID: c.DataType}
	}
	return rel
}

func tupleFrom(rel *Relation, td *pglogrepl.TupleData) (Tuple, error) {
	if td == nil {
		return nil, fmt.Errorf("%s: the message has no tuple", rel.FullName())
	}
	if len(td.Columns) != len(rel.Columns) {
		return nil, fmt.Errorf("%s: the tuple has %d columns but the relation announced %d", rel.FullName(), len(td.Columns), len(rel.Columns))
	}
	t := make(Tuple, len(td.Columns))
	for i, c := range td.Columns {
		switch c.DataType {
		case pglogrepl.TupleDataTypeNull:
			t[i] = Value{Kind: ValueNull}
		case pglogrepl.TupleDataTypeText:
			t[i] = Value{Kind: ValueText, Text: string(c.Data)}
		case pglogrepl.TupleDataTypeToast:
			t[i] = Value{Kind: ValueToast}
		default:
			return nil, fmt.Errorf("%s.%s: unsupported tuple data type %q (binary transfer is not requested)", rel.FullName(), rel.Columns[i].Name, c.DataType)
		}
	}
	return t, nil
}
