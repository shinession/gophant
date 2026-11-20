package multidb

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shinession/gophant/internal/meta"
	"github.com/shinession/gophant/internal/precheck"
)

func dbs(names ...string) []*Database {
	out := make([]*Database, len(names))
	for i, n := range names {
		out[i] = &Database{Name: n}
	}
	return out
}

// gauge records how many calls are inside it at the same time.
type gauge struct {
	cur, peak atomic.Int32
}

func (g *gauge) enter() {
	n := g.cur.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (g *gauge) leave() { g.cur.Add(-1) }

func TestForEachBoundsTheParallelismAndKeepsTheOrderOfTheResults(t *testing.T) {
	var g gauge
	in := dbs("a", "b", "c", "d", "e", "f")
	out := forEach(context.Background(), in, 2, func(_ context.Context, d *Database) (string, error) {
		g.enter()
		defer g.leave()
		time.Sleep(20 * time.Millisecond)
		return "note " + d.Name, nil
	})
	if g.peak.Load() != 2 {
		t.Errorf("at most 2 databases at a time, peak was %d", g.peak.Load())
	}
	for i, o := range out {
		if o.Database != in[i] || o.Note != "note "+in[i].Name || o.Err != nil {
			t.Errorf("result %d = %+v", i, o)
		}
	}
}

func TestForEachRunsASingleDatabaseAtATimeWhenAsked(t *testing.T) {
	var g gauge
	forEach(context.Background(), dbs("a", "b", "c"), 0, func(context.Context, *Database) (string, error) { // 0 is treated as 1
		g.enter()
		defer g.leave()
		time.Sleep(10 * time.Millisecond)
		return "", nil
	})
	if g.peak.Load() != 1 {
		t.Errorf("peak = %d, want 1", g.peak.Load())
	}
}

func TestForEachOneFailureDoesNotStopTheOthers(t *testing.T) {
	boom := errors.New("boom")
	out := forEach(context.Background(), dbs("a", "b", "c"), 1, func(_ context.Context, d *Database) (string, error) {
		if d.Name == "b" {
			return "", boom
		}
		return "", nil
	})
	if out[0].Err != nil || !errors.Is(out[1].Err, boom) || out[2].Err != nil {
		t.Errorf("results = %+v", out)
	}
}

func TestForEachStopsStartingWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	out := forEach(ctx, dbs("a", "b", "c", "d"), 1, func(context.Context, *Database) (string, error) {
		if started.Add(1) == 1 {
			cancel()
		}
		return "", nil
	})
	if started.Load() >= 4 {
		t.Errorf("every database was started although the context ended")
	}
	failed := 0
	for _, o := range out {
		if errors.Is(o.Err, context.Canceled) {
			failed++
		}
	}
	if failed == 0 {
		t.Errorf("the databases that did not start must carry the context error: %+v", out)
	}
}

func TestPipelineStartsAStreamAsSoonAsItsOwnCopyIsDone(t *testing.T) {
	var copies gauge
	streamsRunning := make(chan string, 8)
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex

	jobs := []job{{db: dbs("a")[0], copy: true}, {db: dbs("b")[0], copy: true}, {db: dbs("c")[0], copy: true}}
	done := make(chan []Outcome)
	go func() {
		done <- pipeline(context.Background(), jobs, 1,
			func(_ context.Context, d *Database) error {
				copies.enter()
				defer copies.leave()
				mu.Lock()
				order = append(order, "copy "+d.Name)
				mu.Unlock()
				return nil
			},
			func(_ context.Context, d *Database) (string, error) {
				streamsRunning <- d.Name
				<-release // a stream runs until it is stopped
				return "streamed", nil
			})
	}()

	// With one copy at a time, all three copies must finish while the streams of the earlier
	// databases are still running: a stream does not hold the place of a copy.
	got := map[string]bool{}
	for range 3 {
		select {
		case name := <-streamsRunning:
			got[name] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v streams started: a running stream must not block the next copy", got)
		}
	}
	if copies.peak.Load() != 1 {
		t.Errorf("copies at the same time: %d, want 1", copies.peak.Load())
	}
	close(release)
	out := <-done
	if !slices.Equal(order, []string{"copy a", "copy b", "copy c"}) {
		t.Errorf("the copies ran in the order %v", order)
	}
	for _, o := range out {
		if o.Err != nil || o.Note != "streamed" {
			t.Errorf("outcome = %+v", o)
		}
	}
}

func TestPipelineStreamsThatCanStartAtOnceGoFirst(t *testing.T) {
	var mu sync.Mutex
	var started []string
	jobs := []job{
		{db: dbs("copy1")[0], copy: true},
		{db: dbs("stream1")[0]},
		{db: dbs("copy2")[0], copy: true},
		{db: dbs("stream2")[0]},
	}
	gate := make(chan struct{})
	var once sync.Once
	go func() {
		time.Sleep(200 * time.Millisecond)
		once.Do(func() { close(gate) })
	}()
	pipeline(context.Background(), jobs, 1,
		func(_ context.Context, d *Database) error {
			<-gate // the copy is slow: nothing behind it in the queue may wait for it
			return nil
		},
		func(_ context.Context, d *Database) (string, error) {
			mu.Lock()
			started = append(started, d.Name)
			mu.Unlock()
			return "", nil
		})
	if len(started) != 4 || !(started[0] == "stream1" || started[0] == "stream2") || !(started[1] == "stream1" || started[1] == "stream2") {
		t.Errorf("the streams without a copy must start before the slow copy ends, order: %v", started)
	}
}

func TestPipelineAFailedCopyHasNoStreamAndDoesNotStopTheOthers(t *testing.T) {
	boom := errors.New("the copy failed")
	var streamed sync.Map
	jobs := []job{{db: dbs("a")[0], copy: true}, {db: dbs("b")[0], copy: true}, {db: dbs("c")[0], copy: true}}
	out := pipeline(context.Background(), jobs, 2,
		func(_ context.Context, d *Database) error {
			if d.Name == "b" {
				return boom
			}
			return nil
		},
		func(_ context.Context, d *Database) (string, error) {
			streamed.Store(d.Name, true)
			return "", nil
		})
	if _, ok := streamed.Load("b"); ok {
		t.Error("a database whose copy failed must not stream")
	}
	for _, name := range []string{"a", "c"} {
		if _, ok := streamed.Load(name); !ok {
			t.Errorf("%s must stream although b failed", name)
		}
	}
	var failed []string
	for _, o := range out {
		if o.Err != nil {
			failed = append(failed, o.Database.Name)
			if !errors.Is(o.Err, boom) {
				t.Errorf("error = %v", o.Err)
			}
		}
	}
	if !slices.Equal(failed, []string{"b"}) {
		t.Errorf("failed = %v", failed)
	}
}

func TestPipelineAFailedStreamDoesNotStopTheOthers(t *testing.T) {
	boom := errors.New("constraint violation")
	jobs := []job{{db: dbs("a")[0]}, {db: dbs("b")[0]}}
	var ok atomic.Int32
	out := pipeline(context.Background(), jobs, 1, nil,
		func(_ context.Context, d *Database) (string, error) {
			if d.Name == "a" {
				return "", boom
			}
			ok.Add(1)
			return "", nil
		})
	if ok.Load() != 1 {
		t.Errorf("the healthy stream must run, ok = %d", ok.Load())
	}
	if !errors.Is(out[0].Err, boom) || out[1].Err != nil {
		t.Errorf("outcomes = %+v", out)
	}
}

func TestPipelineStopsStartingCopiesWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var copied atomic.Int32
	jobs := []job{{db: dbs("a")[0], copy: true}, {db: dbs("b")[0], copy: true}, {db: dbs("c")[0], copy: true}}
	out := pipeline(ctx, jobs, 1,
		func(context.Context, *Database) error {
			if copied.Add(1) == 1 {
				cancel()
			}
			return nil
		},
		func(context.Context, *Database) (string, error) { return "", nil })
	if copied.Load() == 3 {
		t.Error("every copy started although the context ended")
	}
	canceled := 0
	for _, o := range out {
		if errors.Is(o.Err, context.Canceled) {
			canceled++
		}
	}
	if canceled == 0 {
		t.Errorf("a database that never started must carry the context error: %+v", out)
	}
}

func TestPhaseOf(t *testing.T) {
	cases := []struct {
		state *meta.SyncState
		want  phase
	}{
		{nil, phaseCopy},
		{&meta.SyncState{Stage: meta.StagePrecheck}, phaseCopy},
		{&meta.SyncState{Stage: meta.StageSnapshot}, phaseCopy},
		{&meta.SyncState{Stage: meta.StageCDCStream}, phaseStream},
		{&meta.SyncState{Stage: meta.StageCutover}, phaseStream},
		{&meta.SyncState{Stage: meta.StageVerify}, phaseDone},
		{&meta.SyncState{Stage: meta.StageDone}, phaseDone},
	}
	for _, c := range cases {
		if got := phaseOf(c.state); got != c.want {
			t.Errorf("phaseOf(%v) = %d, want %d", c.state, got, c.want)
		}
	}
}

// confirmer returns a Confirm function that records the question and answers.
func confirmer(answer bool, asked *[]string) func(string) (bool, error) {
	return func(prompt string) (bool, error) {
		*asked = append(*asked, prompt)
		return answer, nil
	}
}

func reportOf(cluster []precheck.Result, reports ...*DatabaseReport) *Report {
	return &Report{Cluster: cluster, Databases: reports}
}

func TestStartableStopsOnAFailureOfTheCluster(t *testing.T) {
	var asked []string
	f := &Fleet{Confirm: confirmer(true, &asked)}
	rep := reportOf([]precheck.Result{res("source.wal_level", precheck.SeverityFail, "replica")}, dbReport("a", 1))
	if _, _, err := f.startable(rep, 3); err == nil || !strings.Contains(err.Error(), "cluster") {
		t.Errorf("a failure of the cluster must stop everything, got %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("nothing to ask: %v", asked)
	}
}

func TestStartableAsksBeforeItSkipsABlockedDatabase(t *testing.T) {
	good := dbReport("good", 1)
	bad := dbReport("bad", 1, res("source.replica_identity", precheck.SeverityFail, "no key"))
	empty := dbReport("empty", 0)
	empty.Skipped = "no table in scope, nothing to copy"

	var asked []string
	f := &Fleet{Confirm: confirmer(true, &asked)}
	ready, others, err := f.startable(reportOf(nil, good, bad, empty), 0)
	if err != nil {
		t.Fatalf("startable: %v", err)
	}
	if len(ready) != 1 || ready[0].Name != "good" {
		t.Errorf("ready = %v", ready)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "bad") || strings.Contains(asked[0], "good") {
		t.Errorf("the question must name the blocked database only: %v", asked)
	}
	notes := map[string]Outcome{}
	for _, o := range others {
		notes[o.Database.Name] = o
	}
	if notes["bad"].Err == nil || notes["empty"].Err != nil || !strings.HasPrefix(notes["empty"].Note, "skipped") {
		t.Errorf("the other outcomes are wrong: %+v", others)
	}

	// An operator who says no cancels everything.
	asked = nil
	f = &Fleet{Confirm: confirmer(false, &asked)}
	if _, _, err := f.startable(reportOf(nil, good, bad), 0); !errors.Is(err, errCancelled) {
		t.Errorf("a refusal must cancel, got %v", err)
	}
}

func TestStartableDoesNotAskWhenEverythingIsReady(t *testing.T) {
	var asked []string
	f := &Fleet{Confirm: confirmer(false, &asked)}
	ready, _, err := f.startable(reportOf(nil, dbReport("a", 1), dbReport("b", 2)), 0)
	if err != nil || len(ready) != 2 || len(asked) != 0 {
		t.Errorf("ready=%d err=%v asked=%v", len(ready), err, asked)
	}
}

func TestStartableWithNothingToStart(t *testing.T) {
	bad := dbReport("bad", 1, res("source.replica_identity", precheck.SeverityFail, "no key"))
	var asked []string
	f := &Fleet{Confirm: confirmer(true, &asked)}

	// Everything is blocked and nothing streams: say so, do not ask.
	if _, _, err := f.startable(reportOf(nil, bad), 0); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("expected an error naming the blocked database, got %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("nothing to ask: %v", asked)
	}

	// Streams that are running make it worth going on without the blocked database, after asking.
	ready, others, err := f.startable(reportOf(nil, bad), 2)
	if err != nil || len(ready) != 0 || len(others) != 1 || len(asked) != 1 {
		t.Errorf("ready=%d others=%d asked=%d err=%v", len(ready), len(others), len(asked), err)
	}
}

func TestSummarize(t *testing.T) {
	outs := []Outcome{
		{Database: &Database{Name: "zeta"}},
		{Database: &Database{Name: "Alpha"}, Note: "skipped: the snapshot is already complete (stage CDC_STREAM)"},
		{Database: &Database{Name: "crm"}, Err: errors.New("the copy failed after 1 of 3 tables\nlook at status")},
	}
	var buf bytes.Buffer
	err := summarize(&buf, "snapshot", outs)
	if err == nil || !strings.Contains(err.Error(), "snapshot failed for 1 of 3 database(s): crm") {
		t.Errorf("error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Summary of snapshot: 3 database(s), 2 ok, 1 failed",
		"  crm    FAILED: the copy failed after 1 of 3 tables\n",
		"  Alpha  skipped: the snapshot is already complete (stage CDC_STREAM)",
		"  zeta   ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "look at status") {
		t.Errorf("only the first line of an error belongs in the summary:\n%s", out)
	}
	if !(strings.Index(out, "Alpha") < strings.Index(out, "crm") && strings.Index(out, "crm") < strings.Index(out, "zeta")) {
		t.Errorf("the databases must be sorted by name:\n%s", out)
	}
	if outs[0].Database.Name != "zeta" {
		t.Error("summarize must not reorder the slice of the caller")
	}

	buf.Reset()
	if err := summarize(&buf, "sync", outs[:1]); err != nil {
		t.Errorf("no failure, no error: %v", err)
	}
}
