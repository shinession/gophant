// Package multidb migrates every database of the cluster instead of one: it finds the databases on
// s1, checks them together, and runs the stages for each of them, reusing the code of the
// single-database stages. The state of a database stays where it always was, in the gophant_meta
// schema of its copy on s3, so nothing in this package keeps state of its own.
package multidb

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxSlotName is the longest name PostgreSQL accepts for a replication slot.
const maxSlotName = 63

// SlotName returns the replication slot of a database: the configured name, an underscore and the name
// of the database. A slot name is unique in the whole cluster and may only hold lower-case letters,
// digits and underscores, so a database name that does not fit is converted, and a short hash of the
// original name keeps two different names apart ("Sales" and "sales" are two databases).
//
// The configuration limits the base name (see config.Databases), which leaves room for at least 13
// characters of the database name next to the hash.
func SlotName(base, db string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return '_'
	}, db)
	name := base + "_" + clean
	if clean == db && len(name) <= maxSlotName {
		return name
	}
	sum := sha256.Sum256([]byte(db))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if keep := maxSlotName - len(base) - 1 - len(suffix); len(clean) > keep {
		clean = clean[:max(keep, 0)]
	}
	return base + "_" + clean + suffix
}
