package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmer(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		assumeYes bool
		want      bool
		wantErr   bool
	}{
		{"yes", "yes\n", false, true, false},
		{"yes in capitals with spaces", "  YES \n", false, true, false},
		{"anything else is no", "y\n", false, false, false},
		{"empty line is no", "\n", false, false, false},
		{"answer without newline at EOF", "yes", false, true, false},
		{"no input at all", "", false, false, true},
		{"--yes skips the question", "", true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			ok, err := confirmer(strings.NewReader(c.input), &out, c.assumeYes)("Delete everything?")
			if (err != nil) != c.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, c.wantErr)
			}
			if ok != c.want {
				t.Errorf("ok = %v, want %v", ok, c.want)
			}
			if c.assumeYes && out.Len() != 0 {
				t.Errorf("--yes must not print a prompt, got %q", out.String())
			}
		})
	}
}
