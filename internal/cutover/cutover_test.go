package cutover

import (
	"testing"

	"github.com/jackc/pglogrepl"
)

func lsn(v uint64) pglogrepl.LSN { return pglogrepl.LSN(v) }

func TestLagBytes(t *testing.T) {
	cases := []struct {
		current, applied uint64
		want             int64
	}{
		{1000, 400, 600},
		{1000, 1000, 0},
		{1000, 1200, 0}, // applied can never be ahead, but must not go negative
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := lagBytes(lsn(c.current), lsn(c.applied)); got != c.want {
			t.Errorf("lagBytes(%d, %d) = %d, want %d", c.current, c.applied, got, c.want)
		}
	}
}
