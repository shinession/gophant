package multidb

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestLineWriterPrefixesEveryLine(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newLineWriter(&mu, &buf, "Sales-2024")
	fmt.Fprint(w, "first\nsecond")
	fmt.Fprint(w, " half\n\n   \nthird\n")
	if want := "[Sales-2024] first\n[Sales-2024] second half\n[Sales-2024] third\n"; buf.String() != want {
		t.Errorf("got\n%q\nwant\n%q", buf.String(), want)
	}
}

func TestLineWriterHoldsBackAnUnfinishedLineUntilFlush(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newLineWriter(&mu, &buf, "crm")
	fmt.Fprint(w, "no line break")
	if buf.Len() != 0 {
		t.Fatalf("an unfinished line was written: %q", buf.String())
	}
	w.Flush()
	if buf.String() != "[crm] no line break\n" {
		t.Errorf("Flush wrote %q", buf.String())
	}
	w.Flush() // a second flush has nothing left to write
	if buf.String() != "[crm] no line break\n" {
		t.Errorf("a second Flush wrote again: %q", buf.String())
	}
}

// Writers of different databases share one mutex, so lines of databases that run at the same time
// never mix.
func TestLineWritersDoNotMixTheirLines(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d"} {
		w := newLineWriter(&mu, &buf, name)
		wg.Go(func() {
			for i := range 200 {
				fmt.Fprintf(w, "line %d of a rather long report line to make a torn write likely\n", i)
			}
			w.Flush()
		})
	}
	wg.Wait()

	pattern := regexp.MustCompile(`^\[[abcd]\] line \d+ of a rather long report line to make a torn write likely$`)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 800 {
		t.Fatalf("got %d lines, want 800", len(lines))
	}
	for _, l := range lines {
		if !pattern.MatchString(l) {
			t.Fatalf("a line is torn: %q", l)
		}
	}
}
