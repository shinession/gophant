package catalog

import (
	"context"
	"fmt"

	"github.com/shinession/gophant/internal/pg"
)

// FirstNormalObjectID is the first OID handed out to user objects. Every type below it is built
// in and has the same OID in every database, so binary COPY between two databases is safe for it.
const FirstNormalObjectID = 16384

// TypeInfo is the part of a pg_type row that decides whether binary COPY is safe for a column.
type TypeInfo struct {
	OID uint32
	// Kind is pg_type.typtype: 'b' base, 'c' composite, 'd' domain, 'e' enum, 'p' pseudo, 'r' range, 'm' multirange.
	Kind byte
	// Elem is the element type of an array type (pg_type.typelem for a true array), else 0.
	Elem uint32
	// Base is the base type of a domain, else 0.
	Base uint32
	// Fields are the column types of a composite type.
	Fields []uint32
	// Subtype is the element type of a range, or the range of a multirange.
	Subtype uint32
	// HasBinaryIO is false when the type has no binary send or receive function.
	HasBinaryIO bool
}

// BinarySafe reports whether a column of the given type can be moved with COPY (FORMAT BINARY)
// from one database to another.
//
// The binary format is only portable between databases when every type OID that appears inside a
// value is identical on both sides:
//
//   - an array value carries the OID of its element type, and a composite (record) value the OIDs of
//     its field types. For a user-defined type the OID differs between the source and the target
//     database (pg_restore creates the type again), so such values are rejected with
//     "wrong element type" / "wrong data type". Plain enums are fine: they travel as their label.
//   - a type without binary send/receive functions cannot be copied in binary at all.
//
// Domains, ranges and multiranges are safe exactly when the type they wrap is.
func BinarySafe(oid uint32, types map[uint32]*TypeInfo) bool {
	return binarySafe(oid, types, map[uint32]bool{})
}

func binarySafe(oid uint32, types map[uint32]*TypeInfo, visiting map[uint32]bool) bool {
	t, ok := types[oid]
	if !ok {
		// Not loaded: only the built-in types are not loaded, and they are safe.
		return oid < FirstNormalObjectID
	}
	if visiting[oid] {
		return true // recursive type, the verdict comes from the other members of the cycle
	}
	visiting[oid] = true
	defer delete(visiting, oid)

	if oid >= FirstNormalObjectID && !t.HasBinaryIO {
		return false
	}
	if t.Elem != 0 {
		if t.Elem >= FirstNormalObjectID {
			return false
		}
		return binarySafe(t.Elem, types, visiting)
	}
	switch t.Kind {
	case 'd':
		return binarySafe(t.Base, types, visiting)
	case 'r', 'm':
		return binarySafe(t.Subtype, types, visiting)
	case 'c':
		for _, f := range t.Fields {
			if f >= FirstNormalObjectID || !binarySafe(f, types, visiting) {
				return false
			}
		}
	}
	return true
}

const userTypesSQL = `
SELECT t.oid::bigint,
       t.typtype::text,
       CASE WHEN t.typcategory = 'A' THEN t.typelem::bigint ELSE 0 END,
       t.typbasetype::bigint,
       COALESCE(r.rngsubtype, m.rngsubtype, 0)::bigint,
       (t.typsend <> 0 AND t.typreceive <> 0)
FROM pg_type t
LEFT JOIN pg_range r ON r.rngtypid = t.oid
LEFT JOIN pg_range m ON m.rngmultitypid = t.oid
WHERE t.oid >= $1`

const compositeFieldsSQL = `
SELECT t.oid::bigint, a.atttypid::bigint
FROM pg_type t
JOIN pg_attribute a ON a.attrelid = t.typrelid
WHERE t.typtype = 'c' AND t.oid >= $1 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY t.oid, a.attnum`

// LoadUserTypes loads every user-defined type of the database, the input of BinarySafe.
func LoadUserTypes(ctx context.Context, q pg.Querier) (map[uint32]*TypeInfo, error) {
	types := make(map[uint32]*TypeInfo)

	rows, err := q.Query(ctx, userTypesSQL, int64(FirstNormalObjectID))
	if err != nil {
		return nil, fmt.Errorf("load user types: %w", err)
	}
	for rows.Next() {
		var (
			oid, elem, base, sub int64
			kind                 string
			t                    TypeInfo
		)
		if err := rows.Scan(&oid, &kind, &elem, &base, &sub, &t.HasBinaryIO); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan user type: %w", err)
		}
		t.OID, t.Kind, t.Elem, t.Base, t.Subtype = uint32(oid), kind[0], uint32(elem), uint32(base), uint32(sub)
		types[t.OID] = &t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load user types: %w", err)
	}

	rows, err = q.Query(ctx, compositeFieldsSQL, int64(FirstNormalObjectID))
	if err != nil {
		return nil, fmt.Errorf("load composite fields: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid, field int64
		if err := rows.Scan(&oid, &field); err != nil {
			return nil, fmt.Errorf("scan composite field: %w", err)
		}
		if t, ok := types[uint32(oid)]; ok {
			t.Fields = append(t.Fields, uint32(field))
		}
	}
	return types, rows.Err()
}

// AssignCopyFormats decides the COPY format of every table: binary unless a column type is not
// binary safe. Columns must have been loaded. It returns the tables that fall back to text,
// with the offending column, for reporting.
func AssignCopyFormats(tables []Table, types map[uint32]*TypeInfo) []FormatFallback {
	var fallbacks []FormatFallback
	for i := range tables {
		t := &tables[i]
		t.CopyFormat = FormatBinary
		for _, c := range t.CopyColumns() {
			if !BinarySafe(c.TypeOID, types) {
				t.CopyFormat = FormatText
				fallbacks = append(fallbacks, FormatFallback{Table: t.FullName(), Column: c.Name, Type: c.TypeName})
				break
			}
		}
	}
	return fallbacks
}

// FormatFallback names a table that is copied in text format and the first column that forced it.
type FormatFallback struct {
	Table  string
	Column string
	Type   string
}
