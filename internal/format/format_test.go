package format

import "testing"

func TestBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 2048: "2.0 KiB", 5 << 30: "5.0 GiB", 3 << 40: "3.0 TiB"}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestInt(t *testing.T) {
	cases := map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 41503: "41,503", 1234567890: "1,234,567,890", -1234567: "-1,234,567"}
	for in, want := range cases {
		if got := Int(in); got != want {
			t.Errorf("Int(%d) = %q, want %q", in, got, want)
		}
	}
}
