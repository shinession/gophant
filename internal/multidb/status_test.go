package multidb

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shinession/gophant/internal/status"
)

func sampleStatus() *StatusReport {
	lag := int64(2048)
	return &StatusReport{Databases: []DatabaseStatus{
		{Database: "appdb", Info: &status.Info{
			Stage: "CDC_STREAM", LagBytes: &lag,
			Slot: &status.Slot{Name: "gophant_slot_appdb", Exists: true, Active: true, WALStatus: "reserved", RetainedWAL: 3 << 20},
			Tables: []status.Table{
				{Name: "public.a", Status: "DONE"}, {Name: "public.b", Status: "DONE"}, {Name: "public.c", Status: "FAILED"}, {Name: "public.d", Status: "PENDING"},
			},
		}},
		{Database: "Sales-2024"},
		{Database: "postgres", Skipped: skipReasonEmpty},
		{Database: "locked", Error: "connect: permission denied\nsecond line"},
		{Database: "gone", Info: &status.Info{Stage: "CDC_STREAM", Slot: &status.Slot{Name: "gophant_slot_gone"}}},
	}}
}

func TestStatusReportPrint(t *testing.T) {
	var buf bytes.Buffer
	sampleStatus().Print(&buf)
	out := buf.String()
	for _, want := range []string{
		"gophant status (5 database(s))",
		"DATABASE", "STAGE", "LAG", "SLOT", "TABLES",
		"appdb", "CDC_STREAM", "2.0 KiB", "active, reserved, 3.0 MiB kept", "2/4 copied, 1 FAILED",
		"Sales-2024", "not started",
		"postgres", "skipped: " + skipReasonEmpty,
		"locked", "unavailable: connect: permission denied",
		"does not exist on s1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "second line") {
		t.Errorf("only the first line of an error belongs in the table:\n%s", out)
	}
}

func TestStatusReportJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleStatus().WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got struct {
		Databases []struct {
			Database string `json:"database"`
			Skipped  string `json:"skipped"`
			Error    string `json:"error"`
			Status   *struct {
				Stage string `json:"stage"`
			} `json:"status"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, buf.String())
	}
	if len(got.Databases) != 5 || got.Databases[0].Status == nil || got.Databases[0].Status.Stage != "CDC_STREAM" {
		t.Errorf("unexpected JSON: %+v", got)
	}
	if got.Databases[1].Status != nil || got.Databases[2].Skipped != skipReasonEmpty || !strings.Contains(got.Databases[3].Error, "permission denied") {
		t.Errorf("unexpected JSON for the databases without status: %+v", got.Databases)
	}
}
