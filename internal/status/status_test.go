package status

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sample() *Info {
	lag := int64(1234567)
	yes, no := true, false
	return &Info{
		Stage:         "CDC_STREAM",
		UpdatedAt:     time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC),
		ConsistentLSN: "0/1F1BE00",
		AppliedLSN:    "0/25D2D98",
		SourceLSN:     "0/2600000",
		LagBytes:      &lag,
		Slot:          &Slot{Name: "gophant_slot", Exists: true, Active: true, WALStatus: "reserved", RetainedWAL: 5 << 20},
		Tables: []Table{
			{Name: "public.customers", HasPK: true, Identity: "d", Status: "DONE", RowsCopied: 2000, Verified: &yes},
			{Name: "public.events_nopk", HasPK: false, Identity: "f", Status: "FAILED", RowsCopied: 10, Verified: &no, Error: "boom"},
			{Name: "public.orders", HasPK: true, Identity: "d", Status: "RUNNING", RowsCopied: 1234567},
		},
	}
}

func TestPrint(t *testing.T) {
	var buf bytes.Buffer
	sample().Print(&buf)
	out := buf.String()
	for _, want := range []string{
		"stage          : CDC_STREAM",
		"applied_lsn    : 0/25D2D98",
		"lag            : 1,234,567 bytes",
		"slot           : gophant_slot (active=true, wal_status=reserved, retained WAL 5.0 MiB)",
		"public.customers", "DONE", "yes",
		"public.events_nopk", "FAILED", "error: boom",
		"1,234,567",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output misses %q:\n%s", want, out)
		}
	}
}

func TestPrintMarksTheLagAfterTheCutoverAsInformational(t *testing.T) {
	info := sample()
	info.Stage = "VERIFY"
	var buf bytes.Buffer
	info.Print(&buf)
	if !strings.Contains(buf.String(), "(informational: the stream has stopped)") {
		t.Errorf("the lag after the cutover must be marked:\n%s", buf.String())
	}
	buf.Reset()
	sample().Print(&buf)
	if strings.Contains(buf.String(), "informational") {
		t.Errorf("a running stream has a real lag:\n%s", buf.String())
	}
}

func TestPrintHandlesMissingSource(t *testing.T) {
	info := &Info{Stage: "SNAPSHOT", SourceError: "connection refused", Tables: []Table{}}
	var buf bytes.Buffer
	info.Print(&buf)
	out := buf.String()
	for _, want := range []string{"s1             : unavailable: connection refused", "lag            : -", "(none registered yet)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output misses %q:\n%s", want, out)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := sample().WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Info
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the output is not valid JSON: %v\n%s", err, buf.String())
	}
	if back.Stage != "CDC_STREAM" || back.LagBytes == nil || *back.LagBytes != 1234567 || len(back.Tables) != 3 {
		t.Errorf("round trip lost data: %+v", back)
	}
}
