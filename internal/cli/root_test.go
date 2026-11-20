package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootRegistersEveryStage(t *testing.T) {
	root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}
	for _, want := range []string{"precheck", "snapshot", "sync", "cutover", "verify", "status"} {
		if !got[want] {
			t.Errorf("subcommand %q is not registered", want)
		}
	}
}

func TestStageFlags(t *testing.T) {
	root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
	cases := map[string][]string{
		"snapshot": {"restart", "yes"},
		"cutover":  {"yes"},
		"verify":   {"drop-slot", "force"},
		"status":   {"json"},
	}
	for name, flags := range cases {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s has no --%s flag", name, f)
			}
		}
	}
	if root.PersistentFlags().Lookup("config") == nil || root.PersistentFlags().ShorthandLookup("c") == nil {
		t.Error("the persistent -c/--config flag is missing")
	}
}

func TestCommandFailsOnMissingConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewRootCommand("test", &out, &errOut)
	root.SetArgs([]string{"status", "-c", filepath.Join(t.TempDir(), "does-not-exist.yaml")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("expected a read config error, got %v", err)
	}
}

func TestCommandRejectsInvalidLogLevel(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gophant.yaml")
	if err := os.WriteFile(cfg, []byte("source:\n  dsn: a\ntarget:\n  dsn: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"status", "-c", cfg, "--log-level", "loud"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--log-level") {
		t.Fatalf("expected an invalid log level error, got %v", err)
	}
}

func TestNewLoggerFormats(t *testing.T) {
	var buf bytes.Buffer
	l, err := newLogger(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Errorf("json log line expected, got %q", buf.String())
	}
	buf.Reset()
	l, err = newLogger(&buf, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text log line expected, got %q", buf.String())
	}
	if _, err := newLogger(&buf, "info", "xml"); err == nil {
		t.Error("an unknown log format must be rejected")
	}
}
