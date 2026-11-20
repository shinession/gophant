// Package status reports where the migration stands: the stage, the replication lag in bytes and the
// copy and verification progress of every table.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
)

// Table is the progress of one table.
type Table struct {
	Name       string `json:"name"`
	HasPK      bool   `json:"has_pk"`
	Identity   string `json:"replica_identity"`
	Status     string `json:"snapshot_status"`
	RowsCopied int64  `json:"rows_copied"`
	Verified   *bool  `json:"verified_ok,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Slot is the replication slot of s1.
type Slot struct {
	Name        string `json:"name"`
	Exists      bool   `json:"exists"`
	Active      bool   `json:"active"`
	WALStatus   string `json:"wal_status,omitempty"`
	RetainedWAL int64  `json:"retained_wal_bytes"`
}

// Info is the full status.
type Info struct {
	Stage         string    `json:"stage"`
	UpdatedAt     time.Time `json:"updated_at"`
	ConsistentLSN string    `json:"consistent_lsn,omitempty"`
	AppliedLSN    string    `json:"applied_lsn,omitempty"`
	// SourceLSN is pg_current_wal_lsn() of s1; LagBytes is its distance to the applied position.
	SourceLSN   string  `json:"source_lsn,omitempty"`
	LagBytes    *int64  `json:"lag_bytes,omitempty"`
	Slot        *Slot   `json:"slot,omitempty"`
	SourceError string  `json:"source_error,omitempty"`
	Tables      []Table `json:"tables"`
}

// Collect reads the state from s3 and, when src is not nil, the position and the slot from s1. A
// problem with s1 does not hide what s3 knows: it is reported in SourceError.
func Collect(ctx context.Context, cfg *config.Config, src, dst *pgxpool.Pool) (*Info, error) {
	store := meta.NewStore(cfg.Target.MetaSchema)
	state, err := store.GetState(ctx, dst)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errors.New("no migration state found on s3: the migration has not started, run 'gophant snapshot'")
	}
	tables, err := store.ListTables(ctx, dst)
	if err != nil {
		return nil, err
	}

	info := &Info{Stage: string(state.Stage), UpdatedAt: state.UpdatedAt, Tables: []Table{}}
	if state.ConsistentLSN != nil {
		info.ConsistentLSN = state.ConsistentLSN.String()
	}
	if state.AppliedLSN != nil {
		info.AppliedLSN = state.AppliedLSN.String()
	}
	for _, t := range tables {
		info.Tables = append(info.Tables, Table{
			Name: t.FullName(), HasPK: t.HasPK, Identity: t.ReplicaID, Status: t.SnapshotStatus,
			RowsCopied: t.RowsCopied, Verified: t.VerifiedOK, Error: t.ErrorMsg,
		})
	}

	if src != nil {
		if err := info.collectSource(ctx, cfg, src, state); err != nil {
			info.SourceError = err.Error()
		}
	}
	return info, nil
}

func (i *Info) collectSource(ctx context.Context, cfg *config.Config, src *pgxpool.Pool, state *meta.SyncState) error {
	var current string
	if err := src.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&current); err != nil {
		return fmt.Errorf("read pg_current_wal_lsn() on s1: %w", err)
	}
	i.SourceLSN = current
	cur, err := pglogrepl.ParseLSN(current)
	if err != nil {
		return err
	}
	if pos, ok := state.StartLSN(); ok {
		lag := int64(0)
		if cur > pos {
			lag = int64(cur - pos)
		}
		i.LagBytes = &lag
	}

	slot := &Slot{Name: cfg.Source.SlotName}
	err = src.QueryRow(ctx, `SELECT active, COALESCE(wal_status, ''),
       COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::bigint
FROM pg_replication_slots WHERE slot_name = $1`, cfg.Source.SlotName).Scan(&slot.Active, &slot.WALStatus, &slot.RetainedWAL)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read the replication slot of s1: %w", err)
	default:
		slot.Exists = true
	}
	i.Slot = slot
	return nil
}

// WriteJSON writes the status as indented JSON.
func (i *Info) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(i)
}

// Print writes the human readable status.
func (i *Info) Print(w io.Writer) {
	fmt.Fprintln(w, "gophant status")
	fmt.Fprintln(w, strings.Repeat("=", 14))
	fmt.Fprintf(w, "stage          : %s (updated %s)\n", i.Stage, i.UpdatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "consistent_lsn : %s\n", orDash(i.ConsistentLSN))
	fmt.Fprintf(w, "applied_lsn    : %s\n", orDash(i.AppliedLSN))
	fmt.Fprintf(w, "s1 current LSN : %s\n", orDash(i.SourceLSN))
	if i.LagBytes != nil {
		note := ""
		if i.Stage == string(meta.StageVerify) || i.Stage == string(meta.StageDone) {
			// The sync has stopped after the cutover while s1 goes on writing records of its own.
			note = " (informational: the stream has stopped)"
		}
		fmt.Fprintf(w, "lag            : %s bytes%s\n", format.Int(*i.LagBytes), note)
	} else {
		fmt.Fprintln(w, "lag            : -")
	}
	switch {
	case i.Slot == nil:
		fmt.Fprintln(w, "slot           : -")
	case !i.Slot.Exists:
		fmt.Fprintf(w, "slot           : %s (does not exist on s1)\n", i.Slot.Name)
	default:
		fmt.Fprintf(w, "slot           : %s (active=%t, wal_status=%s, retained WAL %s)\n",
			i.Slot.Name, i.Slot.Active, i.Slot.WALStatus, format.Bytes(i.Slot.RetainedWAL))
	}
	if i.SourceError != "" {
		fmt.Fprintf(w, "s1             : unavailable: %s\n", i.SourceError)
	}

	fmt.Fprintln(w, "\ntables:")
	if len(i.Tables) == 0 {
		fmt.Fprintln(w, "  (none registered yet)")
		return
	}
	width := len("TABLE")
	for _, t := range i.Tables {
		width = max(width, len(t.Name))
	}
	fmt.Fprintf(w, "  %-*s  %-3s  %-5s  %-8s  %12s  %s\n", width, "TABLE", "PK", "IDENT", "COPY", "ROWS COPIED", "VERIFIED")
	for _, t := range i.Tables {
		verified := "-"
		if t.Verified != nil {
			verified = map[bool]string{true: "yes", false: "NO"}[*t.Verified]
		}
		pk := "yes"
		if !t.HasPK {
			pk = "no"
		}
		fmt.Fprintf(w, "  %-*s  %-3s  %-5s  %-8s  %12s  %s\n", width, t.Name, pk, t.Identity, t.Status, format.Int(t.RowsCopied), verified)
		if t.Error != "" {
			fmt.Fprintf(w, "  %-*s    error: %s\n", width, "", t.Error)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
