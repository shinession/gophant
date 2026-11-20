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
	for _, want := range []string{"precheck", "snapshot", "sync", "cutover", "verify", "status", "run"} {
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
		"run":      {"yes"},
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
	if root.PersistentFlags().Lookup("database") == nil || root.PersistentFlags().ShorthandLookup("d") == nil {
		t.Error("the persistent -d/--database flag is missing")
	}
}

// writeConfig writes a configuration file for a command that fails before it connects anywhere.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gophant.yaml")
	body := "source:\n  dsn: \"postgres://u:p@127.0.0.1:1/postgres\"\ntarget:\n  dsn: \"postgres://u:p@127.0.0.1:1/postgres\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --database selects among the databases that were found, so it means nothing for a single database.
func TestDatabaseFlagNeedsTheMultiDatabaseMode(t *testing.T) {
	cfg := writeConfig(t, "")
	for _, stage := range []string{"precheck", "snapshot", "sync", "cutover", "verify", "status"} {
		root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
		root.SetArgs([]string{stage, "-c", cfg, "-d", "crm"})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "databases.auto") {
			t.Errorf("%s -d crm without databases.auto: expected an error that names databases.auto, got %v", stage, err)
		}
	}
}

func TestRunNeedsTheMultiDatabaseMode(t *testing.T) {
	root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"run", "-c", writeConfig(t, "")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "databases.auto") {
		t.Fatalf("expected an error that names databases.auto, got %v", err)
	}
}

// With databases.auto the command goes to s1 to find the databases; this one cannot be reached, which
// proves that the multi-database path is taken, and the password must not show up in the error.
func TestMultiDatabaseModeConnectsFirst(t *testing.T) {
	cfg := writeConfig(t, "databases:\n  auto: true\n")
	stages := map[string][]string{
		"precheck": nil, "snapshot": {"--yes"}, "sync": nil, "cutover": {"--yes"}, "verify": nil, "status": nil, "run": {"--yes"},
	}
	for stage, flags := range stages {
		root := NewRootCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
		root.SetArgs(append([]string{stage, "-c", cfg}, flags...))
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "source:") {
			t.Errorf("%s: expected a connection error from the source, got %v", stage, err)
			continue
		}
		if strings.Contains(err.Error(), ":p@") {
			t.Errorf("%s: the error leaks the password: %v", stage, err)
		}
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
