package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

type acceptanceAuditReport struct {
	Scope         string   `json:"scope"`
	Key           string   `json:"idempotency_key"`
	State         string   `json:"state"`
	SessionExists bool     `json:"session_exists"`
	DBInitialized bool     `json:"db_initialized"`
	LaunchClaimed bool     `json:"launch_claimed"`
	Issues        []string `json:"issues,omitempty"`
}

// auditAcceptance performs a read-only registry/session binding comparison.
// It renders only after all checks succeed, preserving audit's zero-byte rule.
func auditAcceptance(ctx context.Context, root, scope, key string, out io.Writer) error {
	reg, err := accept.Open(root)
	if err != nil {
		return err
	}
	status, err := reg.Lookup(scope, key)
	if err != nil {
		return err
	}
	report := acceptanceAuditReport{Scope: scope, Key: key, State: status.State,
		SessionExists: status.SessionExists, DBInitialized: status.DBInitialized, LaunchClaimed: status.Launched}
	if status.State == "not_submitted" {
		report.Issues = []string{"claim_missing"}
		return writeAcceptanceReport(out, report)
	}
	if !status.SessionExists {
		report.Issues = append(report.Issues, "session_missing")
	}
	if !status.DBInitialized {
		report.Issues = append(report.Issues, "db_marker_missing")
	}
	if !status.SessionExists {
		report.Issues = append(report.Issues, "binding_unobservable")
		return writeAcceptanceReport(out, report)
	}
	log, err := sqlite.Open(ctx, reg.SessionPath(scope, key))
	if err != nil {
		return err
	}
	defer log.Close()
	events, err := log.Reader.ReadFrom(ctx, 1)
	if err != nil {
		return err
	}
	var binding executionBinding
	foundStart := false
	for _, event := range events {
		if event.Kind != gen.KindSessionStart {
			continue
		}
		var payload struct {
			Binding executionBinding `json:"execution_binding"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("session/start payload: %w", err)
		}
		binding, foundStart = payload.Binding, true
		if foundStart {
			break
		}
	}
	if !foundStart {
		report.Issues = append(report.Issues, "binding_missing")
	} else {
		if binding.Scope != scope || binding.IdempotencyKey != key {
			report.Issues = append(report.Issues, "binding_key_mismatch")
		}
		if binding.RequestFingerprint != status.Acceptance.Fingerprint {
			report.Issues = append(report.Issues, "fingerprint_mismatch")
		}
		if binding.PolicyHash != status.Acceptance.PolicyHash {
			report.Issues = append(report.Issues, "policy_hash_mismatch")
		}
		if binding.OperationID == "" || binding.RhizomeExecutionID == "" {
			report.Issues = append(report.Issues, "binding_incomplete")
		}
		if eventTrace := firstSessionTrace(events); eventTrace != status.Acceptance.TraceID {
			report.Issues = append(report.Issues, "trace_id_mismatch")
		}
	}
	spawn := false
	sessionEnd := false
	for _, event := range events {
		if event.Kind == gen.KindSubagentSpawn {
			spawn = true
		}
		if event.Kind == gen.KindSessionEnd {
			sessionEnd = true
		}
	}
	if spawn && !status.Launched {
		report.Issues = append(report.Issues, "spawn_without_launch_claim")
	}
	// A launch claim followed by world-assembly failure legitimately produces
	// session/start + session/end without a spawn event. Only an interrupted
	// session lacking both spawn and session/end is inconsistent.
	if status.Launched && !spawn && !sessionEnd {
		report.Issues = append(report.Issues, "launch_without_spawn")
	}
	return writeAcceptanceReport(out, report)
}

func firstSessionTrace(events []gen.EventRecord) string {
	for _, e := range events {
		if e.Kind == gen.KindSessionStart {
			return e.TraceID
		}
	}
	return ""
}

func writeAcceptanceReport(out io.Writer, report acceptanceAuditReport) error {
	if len(report.Issues) > 0 {
		return fmt.Errorf("acceptance audit 불일치: %v", report.Issues)
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(report)
}
