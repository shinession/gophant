package multidb

import (
	"slices"
	"strings"
	"testing"

	"github.com/shinession/gophant/internal/config"
)

func found(names ...string) []Found {
	out := make([]Found, len(names))
	for i, n := range names {
		out[i] = Found{Name: n, CanConnect: true}
	}
	return out
}

func names(fs []Found) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func databasesSection(t *testing.T, extra string) *config.Databases {
	t.Helper()
	return &parse(t, `
source:
  dsn: "postgres://u:p@s1/postgres"
target:
  dsn: "postgres://u:p@s3/postgres"
databases:
  auto: true
`+extra).Databases
}

func TestSelectKeepsEverythingWithoutRules(t *testing.T) {
	sel, err := Select(found("appdb", "crm", "postgres"), databasesSection(t, ""), nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := names(sel.Selected); !slices.Equal(got, []string{"appdb", "crm", "postgres"}) || len(sel.LeftOut) != 0 {
		t.Errorf("selected %v, left out %v", got, sel.LeftOut)
	}
}

func TestSelectAppliesIncludeAndExclude(t *testing.T) {
	d := databasesSection(t, "  include: [\"app*\", \"crm\"]\n  exclude: [\"app_test\"]\n")
	sel, err := Select(found("app_test", "appdb", "crm", "postgres", "scratch"), d, nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := names(sel.Selected); !slices.Equal(got, []string{"appdb", "crm"}) {
		t.Errorf("selected %v", got)
	}
	if !slices.Equal(sel.LeftOut, []string{"app_test", "postgres", "scratch"}) {
		t.Errorf("left out %v", sel.LeftOut)
	}
}

func TestSelectOnlyNarrowsTheSelection(t *testing.T) {
	d := databasesSection(t, "  exclude: [\"postgres\"]\n")
	sel, err := Select(found("appdb", "crm", "postgres"), d, []string{"crm"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := names(sel.Selected); !slices.Equal(got, []string{"crm"}) || !slices.Equal(sel.LeftOut, []string{"appdb", "postgres"}) {
		t.Errorf("selected %v, left out %v", got, sel.LeftOut)
	}

	// The order of the flags does not matter, a repeated flag is harmless.
	sel, err = Select(found("appdb", "crm", "postgres"), d, []string{"crm", "appdb", "crm"})
	if err != nil || !slices.Equal(names(sel.Selected), []string{"appdb", "crm"}) {
		t.Errorf("selected %v, err %v", names(sel.Selected), err)
	}
}

func TestSelectRefusesWhatIsNotPartOfTheMigration(t *testing.T) {
	d := databasesSection(t, "  exclude: [\"postgres\"]\n")
	cases := []struct {
		name string
		only []string
		want string
	}{
		{"unknown", []string{"nothere"}, `"nothere" is not on s1`},
		{"excluded", []string{"postgres"}, `"postgres" is left out by databases.include or databases.exclude`},
	}
	for _, c := range cases {
		_, err := Select(found("appdb", "postgres"), d, c.only)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to contain %q", c.name, err, c.want)
		}
	}
	if _, err := Select(found("appdb", "postgres"), databasesSection(t, "  include: [\"nothing*\"]\n"), nil); err == nil ||
		!strings.Contains(err.Error(), "no database is left") {
		t.Errorf("an empty selection must be an error, got %v", err)
	}
	if _, err := Select(nil, databasesSection(t, ""), nil); err == nil {
		t.Error("a server without databases must be an error")
	}
}

func TestSelectKeepsTheConnectPrivilege(t *testing.T) {
	in := []Found{{Name: "appdb", CanConnect: true}, {Name: "locked", CanConnect: false}}
	sel, err := Select(in, databasesSection(t, ""), nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !sel.Selected[0].CanConnect || sel.Selected[1].CanConnect {
		t.Errorf("the CONNECT privilege was lost: %+v", sel.Selected)
	}
}
