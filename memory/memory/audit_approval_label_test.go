package memory

import (
	"testing"

	"github.com/darkcode/infra/core"
)

// RecordAction used to hardcode approvedBy="human" for any approved
// high/critical-risk action purely from the risk level, with no signal for
// who or what actually approved it — a live reproduction of a real write_file
// approval this audit trail is meant to cover confirmed the gate's own
// no-approver fallback can produce this exact shape of entry with no human
// involved. These tests lock in the honest label.

func TestRecordActionLabelsApprovedHighRiskAsApproverNotHuman(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(al.Shutdown)

	if err := al.RecordAction(core.RoleExecutive, "permission:write_file", "write_file", core.RiskHigh, true, "allow-session"); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	entries := al.GetAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ApprovedBy == "human" {
		t.Fatal("approved_by must not assert \"human\" — the system cannot verify that")
	}
	if entries[0].ApprovedBy != "approver" {
		t.Fatalf("approved_by = %q, want %q", entries[0].ApprovedBy, "approver")
	}
}

func TestRecordActionLabelsDeniedHighRiskAsDenied(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(al.Shutdown)

	if err := al.RecordAction(core.RoleExecutive, "permission:write_file", "write_file", core.RiskCritical, false, "deny"); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	entries := al.GetAll()
	if entries[0].ApprovedBy != "denied" {
		t.Fatalf("approved_by = %q, want %q", entries[0].ApprovedBy, "denied")
	}
}

func TestRecordActionLabelsLowRiskAsPolicy(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(al.Shutdown)

	if err := al.RecordAction(core.RoleExecutive, "chat", "", core.RiskLow, true, "success"); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	entries := al.GetAll()
	if entries[0].ApprovedBy != "policy" {
		t.Fatalf("approved_by = %q, want %q", entries[0].ApprovedBy, "policy")
	}
}
