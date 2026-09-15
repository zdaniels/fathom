package core

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"time"

	"github.com/zdaniels/fathom/pkg/types"
)

// Framework is one of the compliance regimes Fathom can produce a report for.
// Each generates the same shape; consumers slice/dice the JSON output to
// their auditor's preferred template.
type Framework string

const (
	FrameworkSOC2  Framework = "SOC2"
	FrameworkHIPAA Framework = "HIPAA"
	FrameworkGDPR  Framework = "GDPR"
)

// Report is the structured output for a compliance request.
type Report struct {
	Framework   Framework          `json:"framework"`
	Period      Period             `json:"period"`
	GeneratedAt time.Time          `json:"generatedAt"`
	Summary     ReportSummary      `json:"summary"`
	Entries     []types.AuditEntry `json:"entries"`
}

type Period struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ReportSummary struct {
	Total       int `json:"total"`
	Denied      int `json:"denied"`
	Escalations int `json:"escalations"`
}

// Exporter produces compliance reports from a stream of audit entries.
type Exporter struct{}

// NewExporter returns a stateless exporter.
func NewExporter() *Exporter { return &Exporter{} }

// Generate filters entries to the requested time window and produces a Report.
func (e *Exporter) Generate(framework Framework, entries []types.AuditEntry, period Period) Report {
	from, _ := time.Parse(time.RFC3339, period.From)
	to, _ := time.Parse(time.RFC3339, period.To)

	var filtered []types.AuditEntry
	for _, en := range entries {
		if !from.IsZero() && en.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && en.Timestamp.After(to) {
			continue
		}
		filtered = append(filtered, en)
	}
	summary := ReportSummary{Total: len(filtered)}
	for _, en := range filtered {
		switch en.PolicyResult {
		case types.PolicyDeny:
			summary.Denied++
		case types.PolicyEscalate:
			summary.Escalations++
		}
	}
	return Report{
		Framework:   framework,
		Period:      period,
		GeneratedAt: time.Now().UTC(),
		Summary:     summary,
		Entries:     filtered,
	}
}

// ExportJSON serialises a Report to indented JSON.
func (e *Exporter) ExportJSON(r Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ExportCSV flattens entries to one-row-per-event CSV.
func (e *Exporter) ExportCSV(r Report) string {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Write([]string{"timestamp", "userId", "sessionId", "action", "policyResult", "detail"})
	for _, en := range r.Entries {
		detail, _ := json.Marshal(en.Detail)
		w.Write([]string{
			en.Timestamp.Format(time.RFC3339),
			en.UserID,
			en.SessionID,
			string(en.Action),
			string(en.PolicyResult),
			string(detail),
		})
	}
	w.Flush()
	return sb.String()
}
