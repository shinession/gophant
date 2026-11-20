// Package precheck verifies the environment before a migration starts and prints a PASS / WARN /
// FAIL report. A FAIL blocks the migration.
package precheck

import (
	"fmt"
	"io"
	"strings"
)

// Severity is the outcome of one check.
type Severity int

// The three outcomes, ordered by gravity.
const (
	SeverityPass Severity = iota
	SeverityWarn
	SeverityFail
)

func (s Severity) String() string {
	switch s {
	case SeverityPass:
		return "PASS"
	case SeverityWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// maxItemsShown caps the objects listed under a result so a report stays readable.
const maxItemsShown = 15

// Result is the outcome of one check.
type Result struct {
	// ID is the stable identifier of the check, e.g. "source.wal_level".
	ID       string
	Severity Severity
	// Summary is a one-line description of what was found.
	Summary string
	// Items lists the offending objects, one per line.
	Items []string
	// Hint tells how to fix a WARN or FAIL.
	Hint string
	// Fix is a statement that fixes the problem and can be run as it is, when there is one. The
	// multi-database report collects them into one script.
	Fix string
}

// Report is the ordered list of results of one precheck run.
type Report struct {
	Results []Result
	// Tables is the number of tables in scope and Bytes the size of the ones that hold data. The
	// multi-database report lists them for every database.
	Tables int
	Bytes  int64
}

// Add appends results.
func (r *Report) Add(results ...Result) { r.Results = append(r.Results, results...) }

// Counts returns how many results have each severity.
func (r *Report) Counts() (pass, warn, fail int) {
	for _, res := range r.Results {
		switch res.Severity {
		case SeverityPass:
			pass++
		case SeverityWarn:
			warn++
		default:
			fail++
		}
	}
	return pass, warn, fail
}

// Failed reports whether at least one check failed.
func (r *Report) Failed() bool {
	_, _, fail := r.Counts()
	return fail > 0
}

// Print writes the human readable report.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintln(w, "gophant precheck report")
	fmt.Fprintln(w, strings.Repeat("=", 23))
	PrintResults(w, r.Results, false)
	pass, warn, fail := r.Counts()
	fmt.Fprintf(w, "\nSummary: %d passed, %d warning(s), %d failure(s)\n", pass, warn, fail)
	if fail > 0 {
		fmt.Fprintln(w, "Result: BLOCKED - fix every FAIL before starting the migration.")
	} else {
		fmt.Fprintln(w, "Result: OK - the migration can start.")
	}
}

// PrintResults writes one line per result, with the objects and the hint underneath. With onlyProblems
// the checks that passed are left out. The multi-database report uses it for each of its sections.
func PrintResults(w io.Writer, results []Result, onlyProblems bool) {
	idWidth := 0
	for _, res := range results {
		if onlyProblems && res.Severity == SeverityPass {
			continue
		}
		idWidth = max(idWidth, len(res.ID))
	}
	for _, res := range results {
		if onlyProblems && res.Severity == SeverityPass {
			continue
		}
		fmt.Fprintf(w, "[%s] %-*s  %s\n", res.Severity, idWidth, res.ID, res.Summary)
		for i, item := range res.Items {
			if i == maxItemsShown {
				fmt.Fprintf(w, "         ... and %d more\n", len(res.Items)-maxItemsShown)
				break
			}
			fmt.Fprintf(w, "         - %s\n", item)
		}
		if res.Hint != "" && res.Severity != SeverityPass {
			fmt.Fprintf(w, "         hint: %s\n", res.Hint)
		}
	}
}
