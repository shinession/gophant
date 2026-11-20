package sequence

import (
	"math"
	"testing"
)

func TestTarget(t *testing.T) {
	asc := func(last int64) Sequence {
		return Sequence{LastValue: last, IncrementBy: 1, MinValue: 1, MaxValue: math.MaxInt64}
	}
	cases := []struct {
		name   string
		seq    Sequence
		offset int64
		want   int64
	}{
		{"ascending adds the offset", asc(1000), 100, 1100},
		{"zero offset keeps the value", asc(1000), 0, 1000},
		{"ascending is clamped to the maximum", Sequence{LastValue: 995, IncrementBy: 1, MinValue: 1, MaxValue: 1000}, 100, 1000},
		{"ascending that would overflow int64 is clamped", asc(math.MaxInt64 - 5), 100, math.MaxInt64},
		{"smallint sequence is clamped to its own maximum", Sequence{LastValue: 32700, IncrementBy: 1, MinValue: 1, MaxValue: 32767}, 100, 32767},
		{"descending subtracts the offset", Sequence{LastValue: -10, IncrementBy: -1, MinValue: math.MinInt64, MaxValue: -1}, 100, -110},
		{"descending is clamped to the minimum", Sequence{LastValue: 50, IncrementBy: -1, MinValue: 1, MaxValue: 1000}, 100, 1},
		{"descending that would underflow int64 is clamped", Sequence{LastValue: math.MinInt64 + 5, IncrementBy: -1, MinValue: math.MinInt64, MaxValue: -1}, 100, math.MinInt64},
		{"a sequence that steps by 10 is moved by the offset, not by steps", Sequence{LastValue: 500, IncrementBy: 10, MinValue: 1, MaxValue: math.MaxInt64}, 100, 600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Target(c.seq, c.offset); got != c.want {
				t.Errorf("Target = %d, want %d", got, c.want)
			}
		})
	}
}
