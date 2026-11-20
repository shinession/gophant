package catalog

import "testing"

// Type OIDs used by the tests: below FirstNormalObjectID is built in, above is user defined.
const (
	oidInt4      = 23
	oidText      = 25
	oidInt4Array = 1007

	oidEnum        = 20001 // user enum
	oidEnumArray   = 20002 // _enum
	oidComposite   = 20003 // composite of built-in fields only
	oidCompositeU  = 20004 // composite with a user-defined field
	oidDomainInt   = 20005 // domain over int4
	oidDomainEnumA = 20006 // domain over the enum array
	oidRange       = 20007 // range over int4
	oidRangeEnum   = 20008 // range over a type that is not binary safe
	oidNoIO        = 20009 // base type without binary I/O
	oidSelfRef     = 20010 // composite that refers to itself through an array
	oidSelfRefArr  = 20011
	oidCompArray   = 20012 // array of the built-in-only composite
)

func fixtureTypes() map[uint32]*TypeInfo {
	return map[uint32]*TypeInfo{
		oidEnum:        {OID: oidEnum, Kind: 'e', HasBinaryIO: true},
		oidEnumArray:   {OID: oidEnumArray, Kind: 'b', Elem: oidEnum, HasBinaryIO: true},
		oidComposite:   {OID: oidComposite, Kind: 'c', Fields: []uint32{oidInt4, oidText}, HasBinaryIO: true},
		oidCompositeU:  {OID: oidCompositeU, Kind: 'c', Fields: []uint32{oidInt4, oidEnum}, HasBinaryIO: true},
		oidDomainInt:   {OID: oidDomainInt, Kind: 'd', Base: oidInt4, HasBinaryIO: true},
		oidDomainEnumA: {OID: oidDomainEnumA, Kind: 'd', Base: oidEnumArray, HasBinaryIO: true},
		oidRange:       {OID: oidRange, Kind: 'r', Subtype: oidInt4, HasBinaryIO: true},
		oidRangeEnum:   {OID: oidRangeEnum, Kind: 'r', Subtype: oidNoIO, HasBinaryIO: true},
		oidNoIO:        {OID: oidNoIO, Kind: 'b', HasBinaryIO: false},
		oidSelfRef:     {OID: oidSelfRef, Kind: 'c', Fields: []uint32{oidInt4, oidSelfRefArr}, HasBinaryIO: true},
		oidSelfRefArr:  {OID: oidSelfRefArr, Kind: 'b', Elem: oidSelfRef, HasBinaryIO: true},
		oidCompArray:   {OID: oidCompArray, Kind: 'b', Elem: oidComposite, HasBinaryIO: true},
	}
}

func TestBinarySafe(t *testing.T) {
	types := fixtureTypes()
	cases := []struct {
		name string
		oid  uint32
		want bool
	}{
		{"built-in int4", oidInt4, true},
		{"built-in text", oidText, true},
		{"built-in array of int4", oidInt4Array, true},
		{"enum travels as its label", oidEnum, true},
		{"array of enum carries the enum OID", oidEnumArray, false},
		{"composite of built-in fields", oidComposite, true},
		{"composite with a user-defined field", oidCompositeU, false},
		{"domain over a built-in type", oidDomainInt, true},
		{"domain over an unsafe array", oidDomainEnumA, false},
		{"range over a built-in type", oidRange, true},
		{"range over a type without binary I/O", oidRangeEnum, false},
		{"base type without binary I/O", oidNoIO, false},
		{"array of a user-defined composite", oidCompArray, false},
		{"recursive type does not loop", oidSelfRef, false},
	}
	for _, c := range cases {
		if got := BinarySafe(c.oid, types); got != c.want {
			t.Errorf("%s: BinarySafe(%d) = %v, want %v", c.name, c.oid, got, c.want)
		}
	}
}

func TestAssignCopyFormats(t *testing.T) {
	types := fixtureTypes()
	tables := []Table{
		{Schema: "public", Name: "plain", Columns: []Column{
			{Name: "id", TypeOID: oidInt4},
			{Name: "label", TypeOID: oidText},
		}},
		{Schema: "public", Name: "tagged", Columns: []Column{
			{Name: "id", TypeOID: oidInt4},
			{Name: "tags", TypeName: "mood[]", TypeOID: oidEnumArray},
		}},
		{Schema: "public", Name: "generated_only_unsafe", Columns: []Column{
			{Name: "id", TypeOID: oidInt4},
			// A generated column is never copied, so its type must not decide the format.
			{Name: "g", TypeOID: oidEnumArray, Generated: 's'},
		}},
	}
	fallbacks := AssignCopyFormats(tables, types)

	if tables[0].CopyFormat != FormatBinary || tables[2].CopyFormat != FormatBinary {
		t.Errorf("safe tables must stay binary: %v %v", tables[0].CopyFormat, tables[2].CopyFormat)
	}
	if tables[1].CopyFormat != FormatText {
		t.Errorf("a table with an enum array must fall back to text, got %v", tables[1].CopyFormat)
	}
	if len(fallbacks) != 1 || fallbacks[0].Table != "public.tagged" || fallbacks[0].Column != "tags" {
		t.Errorf("unexpected fallbacks: %+v", fallbacks)
	}
}

func TestCopyColumnsSkipsGeneratedColumns(t *testing.T) {
	tbl := Table{Columns: []Column{
		{Name: "a"}, {Name: "b", Generated: 's'}, {Name: "c", Generated: 'v'}, {Name: "d", Identity: 'a'},
	}}
	got := tbl.ColumnNames()
	if len(got) != 2 || got[0] != "a" || got[1] != "d" {
		t.Errorf("ColumnNames = %v, want [a d]", got)
	}
}
