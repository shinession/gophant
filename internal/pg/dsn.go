// Package pg holds the small PostgreSQL helpers shared by every gophant stage:
// connection configuration, DSN handling, identifier quoting and error classification.
package pg

import (
	"fmt"
	"net/url"
	"strings"
)

// SplitPassword removes the password from a connection string and returns it separately.
//
// The external PostgreSQL tools (pg_dump, pg_restore) get the stripped connection string as an
// argument and the password through PGPASSWORD, so that the password never shows up in the process
// list. Both the URL form (postgres://user:pass@host/db) and the keyword/value form
// (host=h user=u password=p) are supported.
func SplitPassword(dsn string) (stripped, password string, err error) {
	dsn = strings.TrimSpace(dsn)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return splitPasswordURL(dsn)
	}
	return splitPasswordKeywords(dsn)
}

// Redact returns the connection string without its password. It is meant for logs and reports.
func Redact(dsn string) string {
	stripped, _, err := SplitPassword(dsn)
	if err != nil {
		return "<unparseable connection string>"
	}
	return stripped
}

func splitPasswordURL(dsn string) (string, string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// url.Error echoes the whole input, which contains the password.
		return "", "", fmt.Errorf("invalid connection URL")
	}
	var password string
	if u.User != nil {
		password, _ = u.User.Password()
		u.User = url.User(u.User.Username())
	}
	q := u.Query()
	if v := q.Get("password"); v != "" {
		password = v
		q.Del("password")
		u.RawQuery = q.Encode()
	}
	return u.String(), password, nil
}

func splitPasswordKeywords(dsn string) (string, string, error) {
	pairs, err := parseKeywordValue(dsn)
	if err != nil {
		return "", "", err
	}
	var password string
	var kept []string
	for _, p := range pairs {
		if p.key == "password" {
			password = p.value
			continue
		}
		kept = append(kept, p.key+"="+quoteKeywordValue(p.value))
	}
	return strings.Join(kept, " "), password, nil
}

type keywordPair struct{ key, value string }

// parseKeywordValue parses the libpq keyword/value syntax: key=value pairs separated by spaces,
// values optionally enclosed in single quotes with \' and \\ escapes.
func parseKeywordValue(s string) ([]keywordPair, error) {
	var pairs []keywordPair
	i := 0
	skipSpaces := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	for {
		skipSpaces()
		if i >= len(s) {
			return pairs, nil
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		key := s[start:i]
		skipSpaces()
		if key == "" || i >= len(s) || s[i] != '=' {
			return nil, fmt.Errorf("invalid keyword/value connection string: expected '=' after %q", key)
		}
		i++ // '='
		skipSpaces()

		var value strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					value.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				value.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("invalid keyword/value connection string: unterminated quote for %q", key)
			}
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
				if s[i] == '\\' && i+1 < len(s) {
					value.WriteByte(s[i+1])
					i += 2
					continue
				}
				value.WriteByte(s[i])
				i++
			}
		}
		pairs = append(pairs, keywordPair{key: key, value: value.String()})
	}
}

func quoteKeywordValue(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n'\\") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}
