// Package verify compares s1 and s3 after the cutover: first the exact row count of every table, then
// fingerprints of primary key ranges (all of them, or a sample, depending on verify.mode).
package verify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/format"
	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/pg"
	"golang.org/x/sync/errgroup"
)

// TableResult is the outcome for one table.
type TableResult struct {
	Name       string
	SourceRows int64
	TargetRows int64
	// Chunks is the number of primary key ranges, Hashed how many of them were fingerprinted.
	Chunks int
	Hashed int
	// Note explains a limited check, for example a table without primary key.
	Note     string
	Problems []string
}

// OK reports whether the table matched.
func (r TableResult) OK() bool { return len(r.Problems) == 0 }

// Report is the outcome of a verification.
type Report struct {
	Mode    string
	Results []TableResult
	Elapsed time.Duration
}

// Failed reports whether any table differs.
func (r *Report) Failed() bool {
	for _, t := range r.Results {
		if !t.OK() {
			return true
		}
	}
	return false
}

// Print writes the human readable report.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "gophant verify report (mode: %s)\n", r.Mode)
	fmt.Fprintln(w, strings.Repeat("=", 40))
	width := 0
	for _, t := range r.Results {
		width = max(width, len(t.Name))
	}
	bad := 0
	var rows int64
	for _, t := range r.Results {
		status := "OK  "
		if !t.OK() {
			status = "DIFF"
			bad++
		}
		rows += t.SourceRows
		detail := fmt.Sprintf("rows %s", format.Int(t.SourceRows))
		if t.SourceRows != t.TargetRows {
			detail = fmt.Sprintf("rows s1=%s s3=%s", format.Int(t.SourceRows), format.Int(t.TargetRows))
		}
		if t.Chunks > 0 {
			detail += fmt.Sprintf(", %d of %d chunk(s) hashed", t.Hashed, t.Chunks)
		}
		if t.Note != "" {
			detail += ", " + t.Note
		}
		fmt.Fprintf(w, "[%s] %-*s  %s\n", status, width, t.Name, detail)
		for _, p := range t.Problems {
			fmt.Fprintf(w, "         - %s\n", p)
		}
	}
	fmt.Fprintf(w, "\nSummary: %d table(s), %s rows on s1, %d with differences (%s)\n",
		len(r.Results), format.Int(rows), bad, r.Elapsed.Round(time.Millisecond))
	if bad > 0 {
		fmt.Fprintln(w, "Result: FAILED - do not switch the application to s3.")
	} else {
		fmt.Fprintln(w, "Result: OK - s3 matches s1.")
	}
}

// Verifier compares the two databases.
type Verifier struct {
	Cfg    *config.Config
	Src    *pgxpool.Pool
	Dst    *pgxpool.Pool
	Logger *slog.Logger
}

// OpenPools opens the pools the verifier needs: one connection per worker on each side, with session
// settings that make the text form of the values identical on both servers.
func OpenPools(ctx context.Context, cfg *config.Config) (src, dst *pgxpool.Pool, err error) {
	n := int32(cfg.Verify.Workers + 1)
	src, err = pg.NewPool(ctx, cfg.Source.DSN, "gophant-verify", n, pg.WithAfterConnect(setSession))
	if err != nil {
		return nil, nil, fmt.Errorf("source: %w", err)
	}
	dst, err = pg.NewPool(ctx, cfg.Target.DSN, "gophant-verify", n, pg.WithAfterConnect(setSession))
	if err != nil {
		src.Close()
		return nil, nil, fmt.Errorf("target: %w", err)
	}
	return src, dst, nil
}

// tablePlan is the chunking of one table.
type tablePlan struct {
	index  int
	name   string // quoted, schema-qualified
	pk     []pkColumn
	chunks []chunk
	picked []int
}

// Run verifies every table of table_progress and records the result of each in the meta store. It
// compares row counts first. Unless the mode is count_only it then splits every table with a primary
// key into ranges of verify.chunk_size rows and compares fingerprints of all of them (full_hash) or of
// a random sample (count_and_sample). A table without primary key is only hashed as a whole in full_hash mode.
func (v *Verifier) Run(ctx context.Context, store *meta.Store) (*Report, error) {
	started := time.Now()
	tables, err := store.ListTables(ctx, v.Dst)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("no tables to verify: %s.table_progress is empty", v.Cfg.Target.MetaSchema)
	}

	results := make([]TableResult, len(tables))
	plans := make([]*tablePlan, len(tables))
	for i, t := range tables {
		results[i] = TableResult{Name: t.FullName()}
		plans[i] = &tablePlan{index: i, name: pg.Qualified(t.Schema, t.Name)}
	}

	// Phase 1: row counts and chunk boundaries, one task per table.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(v.Cfg.Verify.Workers)
	for i := range tables {
		g.Go(func() error { return v.plan(gctx, plans[i], &results[i]) })
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Phase 2: fingerprints, one task per chunk.
	var mu sync.Mutex
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(v.Cfg.Verify.Workers)
	for _, p := range plans {
		for _, ci := range p.picked {
			g.Go(func() error {
				problem, err := v.compare(gctx, p, p.chunks[ci])
				mu.Lock()
				defer mu.Unlock()
				results[p.index].Hashed++
				if problem != "" {
					results[p.index].Problems = append(results[p.index].Problems, problem)
				}
				return err
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	for i, t := range tables {
		msg := strings.Join(results[i].Problems, "; ")
		if err := store.SetVerified(ctx, v.Dst, t.Schema, t.Name, results[i].OK(), msg); err != nil {
			return nil, err
		}
	}
	return &Report{Mode: v.Cfg.Verify.Mode, Results: results, Elapsed: time.Since(started)}, nil
}

// plan counts the rows of both sides and, when hashing is on, prepares the chunks of the table.
func (v *Verifier) plan(ctx context.Context, p *tablePlan, res *TableResult) error {
	countSQL := "SELECT count(*) FROM ONLY " + p.name
	var srcRows, dstRows int64
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return v.Src.QueryRow(gctx, countSQL).Scan(&srcRows) })
	g.Go(func() error { return v.Dst.QueryRow(gctx, countSQL).Scan(&dstRows) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("count the rows of %s: %w", res.Name, err)
	}
	res.SourceRows, res.TargetRows = srcRows, dstRows
	if srcRows != dstRows {
		res.Problems = append(res.Problems, fmt.Sprintf("row count differs: s1 has %s, s3 has %s", format.Int(srcRows), format.Int(dstRows)))
	}
	if v.Cfg.Verify.Mode == config.VerifyModeCountOnly {
		return nil
	}

	pk, err := primaryKey(ctx, v.Src, p.name)
	if err != nil {
		return err
	}
	p.pk = pk
	if len(pk) == 0 {
		// No key to cut ranges from: the whole table is one chunk, hashed only when a full check was asked for.
		p.chunks = []chunk{{}}
		if v.Cfg.Verify.Mode == config.VerifyModeFullHash {
			p.picked = []int{0}
			res.Note = "no primary key, hashed as one chunk"
		} else {
			res.Note = "no primary key, counted only"
		}
		res.Chunks = 1
		return nil
	}
	bounds, err := boundaries(ctx, v.Src, p.name, pk, v.Cfg.Verify.ChunkSize)
	if err != nil {
		return err
	}
	p.chunks = chunksFromBounds(bounds)
	p.picked = pickChunks(len(p.chunks), v.Cfg.Verify.Mode, v.Cfg.Verify.SampleRatio)
	res.Chunks = len(p.chunks)
	return nil
}

// compare fingerprints one chunk on both sides. A difference is returned as a problem text; only a
// failure to run the queries is an error.
func (v *Verifier) compare(ctx context.Context, p *tablePlan, c chunk) (problem string, err error) {
	var srcFP, dstFP fingerprint
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { srcFP, err = fingerprintOf(gctx, v.Src, p.name, p.pk, c); return })
	g.Go(func() (err error) { dstFP, err = fingerprintOf(gctx, v.Dst, p.name, p.pk, c); return })
	if err := g.Wait(); err != nil {
		return "", fmt.Errorf("fingerprint %s (%s): %w", p.name, c, err)
	}
	if srcFP != dstFP {
		return fmt.Sprintf("content differs in %s: s1 has %d rows, s3 has %d rows, the row hashes do not match", c, srcFP.Rows, dstFP.Rows), nil
	}
	return "", nil
}
