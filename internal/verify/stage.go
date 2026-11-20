package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/snapshot"
)

// StageOptions are the switches of the verify command.
type StageOptions struct {
	// DropSlot drops the replication slot of s1 after a successful verification, which releases the
	// WAL that s1 keeps for it, and moves the migration to DONE.
	DropSlot bool
	// Force compares even though the migration has not reached the VERIFY stage.
	Force bool
}

// StageResult says what RunStage did.
type StageResult struct {
	Report *Report
	// Rehearsal is set when the migration was not yet in VERIFY or DONE: the comparison was only a
	// look, differences are expected while s1 is written.
	Rehearsal bool
	// Dropped is set when the slot was dropped and the migration is DONE.
	Dropped bool
}

// RunStage verifies the database that cfg points at: it checks that the migration is in a stage where
// that makes sense, compares s1 and s3, prints the report to out and, with opts.DropSlot, ends the
// migration. It is the whole of the verify command for one database; the multi-database mode calls it
// once per database.
//
// The result is returned together with an error when the comparison found differences, so the caller
// can still see the report.
func RunStage(ctx context.Context, cfg *config.Config, logger *slog.Logger, out io.Writer, opts StageOptions) (*StageResult, error) {
	src, dst, err := OpenPools(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	defer dst.Close()

	store := meta.NewStore(cfg.Target.MetaSchema)
	state, err := store.GetState(ctx, dst)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errors.New("no migration state found on s3: nothing to verify")
	}
	rehearsal := state.Stage != meta.StageVerify && state.Stage != meta.StageDone
	if rehearsal && !opts.Force {
		return nil, fmt.Errorf("the migration is in stage %s: run the cutover first, or pass --force to compare anyway "+
			"(rows that s1 is still writing will show up as differences)", state.Stage)
	}
	if opts.DropSlot && rehearsal {
		return nil, fmt.Errorf("--drop-slot is only allowed in stage %s or %s, the migration is in stage %s", meta.StageVerify, meta.StageDone, state.Stage)
	}

	v := &Verifier{Cfg: cfg, Src: src, Dst: dst, Logger: logger}
	report, err := v.Run(ctx, store)
	if err != nil {
		return nil, err
	}
	report.Print(out)
	res := &StageResult{Report: report, Rehearsal: rehearsal}
	if report.Failed() {
		return res, errors.New("the verification found differences between s1 and s3")
	}
	if rehearsal || !opts.DropSlot {
		return res, nil
	}
	if err := snapshot.DropSlot(ctx, src, cfg.Source.SlotName, logger); err != nil {
		return res, err
	}
	if err := store.SetStage(ctx, dst, meta.StageDone); err != nil {
		return res, err
	}
	res.Dropped = true
	fmt.Fprintf(out, "\nThe replication slot %s was dropped and the migration is DONE.\n", cfg.Source.SlotName)
	return res, nil
}
