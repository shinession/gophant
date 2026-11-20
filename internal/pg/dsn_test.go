package pg

import (
	"strings"
	"testing"
)

func TestSplitPassword(t *testing.T) {
	cases := []struct {
		name         string
		dsn          string
		wantStripped string
		wantPassword string
	}{
		{
			name:         "url with password",
			dsn:          "postgres://gophant:pa%24%24@s1:5432/appdb?sslmode=prefer",
			wantStripped: "postgres://gophant@s1:5432/appdb?sslmode=prefer",
			wantPassword: "pa$$",
		},
		{
			name:         "url without password",
			dsn:          "postgresql://gophant@s1/appdb",
			wantStripped: "postgresql://gophant@s1/appdb",
			wantPassword: "",
		},
		{
			name:         "url with password query parameter",
			dsn:          "postgres://gophant@s1/appdb?password=secret&sslmode=require",
			wantStripped: "postgres://gophant@s1/appdb?sslmode=require",
			wantPassword: "secret",
		},
		{
			name:         "keyword value",
			dsn:          "host=s1 port=5432 user=gophant password=secret dbname=appdb",
			wantStripped: "host=s1 port=5432 user=gophant dbname=appdb",
			wantPassword: "secret",
		},
		{
			name:         "keyword value with quotes",
			dsn:          `host=s1 password='it\'s a "secret"' options='-c search_path=a b'`,
			wantStripped: `host=s1 options='-c search_path=a b'`,
			wantPassword: `it's a "secret"`,
		},
		{
			name:         "keyword value without password",
			dsn:          "host=s1 dbname=appdb",
			wantStripped: "host=s1 dbname=appdb",
			wantPassword: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stripped, password, err := SplitPassword(c.dsn)
			if err != nil {
				t.Fatalf("SplitPassword: %v", err)
			}
			if stripped != c.wantStripped {
				t.Errorf("stripped = %q, want %q", stripped, c.wantStripped)
			}
			if password != c.wantPassword {
				t.Errorf("password = %q, want %q", password, c.wantPassword)
			}
		})
	}
}

func TestSplitPasswordRejectsMalformedInput(t *testing.T) {
	for _, dsn := range []string{"host", "host=s1 password='unterminated", "=value"} {
		if _, _, err := SplitPassword(dsn); err == nil {
			t.Errorf("SplitPassword(%q) should fail", dsn)
		}
	}
}

func TestSplitPasswordErrorDoesNotLeakTheSecret(t *testing.T) {
	_, _, err := SplitPassword("postgres://u:topsecret@host:notaport/db")
	if err == nil {
		t.Fatal("expected an error for an invalid port")
	}
	if got := err.Error(); strings.Contains(got, "topsecret") {
		t.Errorf("error leaks the password: %q", got)
	}
}

func TestRedact(t *testing.T) {
	got := Redact("postgres://u:topsecret@s1:5432/appdb")
	if got != "postgres://u@s1:5432/appdb" {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("host=s1 password=topsecret"); got != "host=s1" {
		t.Errorf("Redact keyword form = %q", got)
	}
}
