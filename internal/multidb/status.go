package multidb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
	"github.com/shinession/gophant/internal/status"
)

// DatabaseStatus is the status of one database. Info is nil for a database whose migration has not started.
type DatabaseStatus struct {
	Database string       `json:"database"`
	Info     *status.Info `json:"status,omitempty"`
	// Skipped is why a database that has not started never will: it has nothing to copy.
	Skipped string `json:"skipped,omitempty"`
	// Error is why the status could not be read.
	Error string `json:"error,omitempty"`
}

// StatusReport is the status of every selected database.
type StatusReport struct {
	Databases []DatabaseStatus `json:"databases"`
}

// Status reads the state of every selected database from s3 and the slot and the lag from s1. A
// database that cannot be read does not hide the others.
func (f *Fleet) Status(ctx context.Context) (*StatusReport, error) {
	onTarget, err := f.targetDatabases(ctx)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{}
	for _, d := range f.Databases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rep.Databases = append(rep.Databases, f.statusOne(ctx, d, onTarget[d.Name]))
	}
	return rep, nil
}

func (f *Fleet) statusOne(ctx context.Context, d *Database, onTarget bool) DatabaseStatus {
	out := DatabaseStatus{Database: d.Name}
	if !onTarget {
		f.markSkipped(ctx, d, &out)
		return out // not started: s3 does not even have the database
	}
	dst, err := pg.NewPool(ctx, d.Cfg.Target.DSN, "gophant-status", 2)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer dst.Close()
	// s1 is optional: what s3 knows is still worth showing when the database of s1 cannot be reached.
	var src *pgxpool.Pool
	if src, err = pg.NewPool(ctx, d.Cfg.Source.DSN, "gophant-status", 2); err != nil {
		d.Logger.Warn("the database is not reachable on s1, showing what s3 knows", "error", err)
		src = nil
	} else {
		defer src.Close()
	}
	state, err := meta.NewStore(d.Cfg.Target.MetaSchema).GetState(ctx, dst)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if state == nil {
		f.markSkipped(ctx, d, &out)
		return out
	}
	info, err := status.Collect(ctx, d.Cfg, src, dst)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Info = info
	return out
}

// markSkipped notes that a database that has not started has nothing to copy, which is not the same as
// a database that has tables and is waiting.
func (f *Fleet) markSkipped(ctx context.Context, d *Database, out *DatabaseStatus) {
	if empty, err := f.nothingToMigrate(ctx, d); err == nil && empty {
		out.Skipped = skipReasonEmpty
	}
}

// WriteJSON writes the status as indented JSON.
func (r *StatusReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Print writes one line per database.
func (r *StatusReport) Print(w io.Writer) {
	title := fmt.Sprintf("gophant status (%d database(s))", len(r.Databases))
	fmt.Fprintln(w, title)
	fmt.Fprintln(w, strings.Repeat("=", len(title)))
	width := len("DATABASE")
	for _, d := range r.Databases {
		width = max(width, len(d.Database))
	}
	fmt.Fprintf(w, "%-*s  %-10s  %12s  %-34s  %s\n", width, "DATABASE", "STAGE", "LAG", "SLOT", "TABLES")
	for _, d := range r.Databases {
		stage, lag, slot, tables := "-", "-", "-", "-"
		switch {
		case d.Error != "":
			tables = "unavailable: " + firstLineOf(d.Error)
		case d.Skipped != "":
			tables = "skipped: " + d.Skipped
		case d.Info == nil:
			tables = "not started"
		default:
			stage = d.Info.Stage
			if d.Info.LagBytes != nil {
				lag = format.Bytes(*d.Info.LagBytes)
			}
			slot = slotText(d.Info)
			tables = tablesText(d.Info)
		}
		fmt.Fprintf(w, "%-*s  %-10s  %12s  %-34s  %s\n", width, d.Database, stage, lag, slot, tables)
	}
}

func slotText(i *status.Info) string {
	switch {
	case i.Slot == nil:
		return "-"
	case !i.Slot.Exists:
		return "does not exist on s1"
	}
	active := "idle"
	if i.Slot.Active {
		active = "active"
	}
	return fmt.Sprintf("%s, %s, %s kept", active, i.Slot.WALStatus, format.Bytes(i.Slot.RetainedWAL))
}

func tablesText(i *status.Info) string {
	var copied, failed int
	for _, t := range i.Tables {
		switch t.Status {
		case meta.StatusDone:
			copied++
		case meta.StatusFailed:
			failed++
		}
	}
	text := fmt.Sprintf("%d/%d copied", copied, len(i.Tables))
	if failed > 0 {
		text += fmt.Sprintf(", %d FAILED", failed)
	}
	return text
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
