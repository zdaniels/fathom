package core

import (
	"strings"
	"testing"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

func mkEntry(ts time.Time, user string, action types.AuditAction, result types.PolicyDecision) types.AuditEntry {
	return types.AuditEntry{
		Timestamp:    ts,
		UserID:       user,
		SessionID:    "sess",
		Action:       action,
		PolicyResult: result,
		Detail:       map[string]interface{}{"k": "v"},
	}
}

func TestComplianceReportFiltersByPeriod(t *testing.T) {
	e := NewExporter()
	now := time.Now().UTC()
	entries := []types.AuditEntry{
		mkEntry(now.Add(-48*time.Hour), "u", types.AuditToolCall, types.PolicyAllow),
		mkEntry(now.Add(-1*time.Hour), "u", types.AuditToolCall, types.PolicyDeny),
		mkEntry(now.Add(-30*time.Minute), "u", types.AuditToolCall, types.PolicyEscalate),
	}
	period := Period{
		From: now.Add(-24 * time.Hour).Format(time.RFC3339),
		To:   now.Format(time.RFC3339),
	}
	r := e.Generate(FrameworkSOC2, entries, period)
	if r.Summary.Total != 2 {
		t.Errorf("Total = %d, want 2 (one entry outside window)", r.Summary.Total)
	}
	if r.Summary.Denied != 1 {
		t.Errorf("Denied = %d, want 1", r.Summary.Denied)
	}
	if r.Summary.Escalations != 1 {
		t.Errorf("Escalations = %d, want 1", r.Summary.Escalations)
	}
	if r.Framework != FrameworkSOC2 {
		t.Errorf("Framework = %v, want SOC2", r.Framework)
	}
}

func TestComplianceReportEmptyPeriodIncludesAll(t *testing.T) {
	e := NewExporter()
	entries := []types.AuditEntry{
		mkEntry(time.Now(), "u", types.AuditToolCall, types.PolicyAllow),
		mkEntry(time.Now(), "u", types.AuditToolCall, types.PolicyAllow),
	}
	r := e.Generate(FrameworkHIPAA, entries, Period{})
	if r.Summary.Total != 2 {
		t.Errorf("empty period should include all entries, got %d", r.Summary.Total)
	}
}

func TestComplianceExportJSONRoundTrips(t *testing.T) {
	e := NewExporter()
	entries := []types.AuditEntry{
		mkEntry(time.Now().UTC(), "u", types.AuditToolCall, types.PolicyAllow),
	}
	r := e.Generate(FrameworkGDPR, entries, Period{})
	b, err := e.ExportJSON(r)
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	if !strings.Contains(string(b), `"framework": "GDPR"`) {
		t.Errorf("JSON missing framework field: %s", b)
	}
}

func TestComplianceExportCSVHasHeaderAndRow(t *testing.T) {
	e := NewExporter()
	entries := []types.AuditEntry{
		mkEntry(time.Now().UTC(), "alice", types.AuditToolCall, types.PolicyAllow),
		mkEntry(time.Now().UTC(), "bob", types.AuditPolicyDecision, types.PolicyDeny),
	}
	r := e.Generate(FrameworkSOC2, entries, Period{})
	csv := e.ExportCSV(r)
	if !strings.HasPrefix(csv, "timestamp,userId,sessionId,action,policyResult,detail") {
		t.Errorf("CSV missing header row: %s", csv[:80])
	}
	if !strings.Contains(csv, "alice") || !strings.Contains(csv, "bob") {
		t.Error("CSV missing one of the entries")
	}
}
