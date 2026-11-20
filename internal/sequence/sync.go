// Package sequence copies the sequence positions of s1 to s3 during the cutover. Sequences are not
// part of the WAL stream (nextval is not replicated), so without this step s3 would hand out values
// that s1 has already used.
package sequence

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// Sequence is the state of one sequence on s1.
type Sequence struct {
	Schema      string
	Name        string
	LastValue   int64
	IncrementBy int64
	MinValue    int64
	MaxValue    int64
}

// FullName returns schema.name.
func (s Sequence) FullName() string { return s.Schema + "." + s.Name }

const listSQL = `
SELECT schemaname, sequencename, last_value, increment_by, min_value, max_value
FROM pg_sequences
WHERE last_value IS NOT NULL
  AND schemaname NOT IN ('pg_catalog', 'information_schema')
  AND schemaname NOT LIKE 'pg\_toast%'
  AND ($1::text[] IS NULL OR schemaname = ANY ($1))
ORDER BY schemaname, sequencename`

// List returns the sequences of the selected schemas that have been used. A sequence whose
// last_value is NULL was never called and keeps its start value on s3.
func List(ctx context.Context, src pg.Querier, filter *config.Filter) ([]Sequence, error) {
	var schemas any
	if len(filter.IncludeSchemas) > 0 {
		schemas = filter.IncludeSchemas
	}
	rows, err := src.Query(ctx, listSQL, schemas)
	if err != nil {
		return nil, fmt.Errorf("list the sequences of s1: %w", err)
	}
	defer rows.Close()
	var out []Sequence
	for rows.Next() {
		var s Sequence
		if err := rows.Scan(&s.Schema, &s.Name, &s.LastValue, &s.IncrementBy, &s.MinValue, &s.MaxValue); err != nil {
			return nil, fmt.Errorf("scan a sequence of s1: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Target returns the value s3 is set to: the last value of s1 moved by a safety offset in the
// direction of the sequence and kept inside the bounds of the sequence. The offset leaves room for
// values that s1 handed out but that never made it into a committed row.
func Target(s Sequence, offset int64) int64 {
	if s.IncrementBy < 0 {
		v := s.LastValue - offset
		if v > s.LastValue || v < s.MinValue { // the subtraction wrapped around, or left the range
			return s.MinValue
		}
		return v
	}
	v := s.LastValue + offset
	if v < s.LastValue || v > s.MaxValue {
		return s.MaxValue
	}
	return v
}

// Result summarizes a synchronization.
type Result struct {
	Synced  int
	Missing []string
}

// setvalSQL sets the sequence and returns the new value, or NULL when the sequence does not exist on s3.
const setvalSQL = `SELECT CASE WHEN to_regclass($1) IS NULL THEN NULL ELSE setval(to_regclass($1), $2::bigint, true) END`

// Sync reads every used sequence of s1 and sets it on s3 with setval(..., true). The operation is
// idempotent. A sequence that does not exist on s3 (for example one that belongs to an excluded
// table) is reported in Result.Missing and does not stop the others.
func Sync(ctx context.Context, src, dst pg.Querier, filter *config.Filter, offset int64, logger *slog.Logger) (Result, error) {
	seqs, err := List(ctx, src, filter)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, s := range seqs {
		target := Target(s, offset)
		var set *int64
		if err := dst.QueryRow(ctx, setvalSQL, pg.Qualified(s.Schema, s.Name), target).Scan(&set); err != nil {
			return res, fmt.Errorf("set the sequence %s on s3: %w", s.FullName(), err)
		}
		if set == nil {
			res.Missing = append(res.Missing, s.FullName())
			logger.Warn("the sequence does not exist on s3, skipped", "sequence", s.FullName())
			continue
		}
		res.Synced++
		logger.Debug("sequence synchronized", "sequence", s.FullName(), "s1_last_value", s.LastValue, "s3_value", target)
	}
	return res, nil
}
