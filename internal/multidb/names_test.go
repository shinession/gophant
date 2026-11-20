package multidb

import (
	"regexp"
	"strings"
	"testing"
)

// slotPattern is what PostgreSQL accepts as the name of a replication slot.
var slotPattern = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)

func TestSlotNameKeepsAPlainName(t *testing.T) {
	if got := SlotName("gophant_slot", "appdb"); got != "gophant_slot_appdb" {
		t.Errorf("SlotName = %q", got)
	}
	if got := SlotName("gophant_slot", "crm_2024"); got != "gophant_slot_crm_2024" {
		t.Errorf("SlotName = %q", got)
	}
}

func TestSlotNameIsValidAndDistinctForAwkwardNames(t *testing.T) {
	names := []string{
		"Sales", "sales", "SALES", // the same letters in another case are another database
		"Sales-2024", "sales_2024", "sales 2024", "sales.2024",
		"ünïcode", "日本語", "💾",
		"a", "_", "1",
		strings.Repeat("x", 63), strings.Repeat("x", 64), strings.Repeat("x", 200),
		strings.Repeat("y", 200),
		"a-" + strings.Repeat("z", 100), "a-" + strings.Repeat("z", 101),
	}
	for _, base := range []string{"gophant_slot", "g", strings.Repeat("b", 40)} {
		seen := map[string]string{}
		for _, db := range names {
			got := SlotName(base, db)
			if !slotPattern.MatchString(got) {
				t.Errorf("SlotName(%q, %q) = %q is not a valid slot name (%d characters)", base, db, got, len(got))
			}
			if got != SlotName(base, db) {
				t.Errorf("SlotName(%q, %q) is not stable", base, db)
			}
			if other, dup := seen[got]; dup {
				t.Errorf("SlotName(%q, ...) gives %q for both %q and %q", base, got, other, db)
			}
			seen[got] = db
		}
	}
}

func TestSlotNameOfALongNameKeepsTheStartOfIt(t *testing.T) {
	got := SlotName("gophant_slot", "customer_relationship_management_database_of_the_northern_region_2024")
	if !strings.HasPrefix(got, "gophant_slot_customer_relationship") || len(got) != maxSlotName {
		t.Errorf("SlotName = %q (%d characters)", got, len(got))
	}
}

func TestSlotNameNeverExceedsTheLimit(t *testing.T) {
	for n := range 120 {
		db := strings.Repeat("d", n)
		if got := SlotName(strings.Repeat("b", 40), db); len(got) > maxSlotName {
			t.Fatalf("a database name of %d characters gives a slot name of %d characters: %q", n, len(got), got)
		}
	}
	// And a case change at any position changes the result.
	seen := map[string]bool{}
	for i := range 8 {
		b := []byte("abcdefgh")
		b[i] = b[i] - 'a' + 'A'
		name := SlotName("s", string(b))
		if seen[name] {
			t.Errorf("two spellings share the slot %q", name)
		}
		seen[name] = true
	}
}
