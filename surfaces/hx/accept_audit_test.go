package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/seams/accept"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func TestAuditAcceptanceHealthyAndDeterministic(t *testing.T) {
	f := newProductionFixture(t)
	if _, err := f.run(t, f.request); err != nil {
		t.Fatal(err)
	}
	reg, err := accept.Open(f.acceptRoot)
	if err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if err := auditAcceptance(context.Background(), f.acceptRoot, f.request.Scope, f.request.IdempotencyKey, &a); err != nil {
		t.Fatal(err)
	}
	if err := auditAcceptance(context.Background(), f.acceptRoot, f.request.Scope, f.request.IdempotencyKey, &b); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() || a.Len() == 0 {
		t.Fatalf("결정적 보고서 위반: %q / %q", a.String(), b.String())
	}
	_ = reg
}

func TestAuditAcceptanceBindingMismatches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*executionBinding)
	}{
		{"fingerprint", func(b *executionBinding) { b.RequestFingerprint = "bbbb" }},
		{"policy", func(b *executionBinding) { b.PolicyHash = "p2" }},
		{"trace", func(b *executionBinding) {}},
		{"missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root := dir + "/accept"
			scope, key := "s", "k"
			reg, err := accept.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = reg.Claim(accept.Acceptance{Scope: scope, Key: key, Fingerprint: "aaaa", TraceID: "11111111111111111111111111111111", PolicyHash: "p1", OperationID: "op"}); err != nil {
				t.Fatal(err)
			}
			log, err := sqlite.Open(context.Background(), reg.SessionPath(scope, key))
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{}`)
			if tc.mutate != nil {
				b := executionBinding{Scope: scope, IdempotencyKey: key, RequestFingerprint: "aaaa", PolicyHash: "p1"}
				tc.mutate(&b)
				raw, _ := json.Marshal(struct {
					Binding executionBinding `json:"execution_binding"`
				}{b})
				payload = raw
			}
			trace := "11111111111111111111111111111111"
			if tc.name == "trace" {
				trace = "22222222222222222222222222222222"
			}
			if err := log.Writer.InitBatch(context.Background(), []gen.EventRecord{{Ts: 1, TraceID: trace, SpanID: "1111111111111111", Kind: gen.KindSessionStart, Actor: "parent", Payload: payload}}); err != nil {
				t.Fatal(err)
			}
			log.Close()
			if err := reg.MarkDBInitialized(scope, key); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = auditAcceptance(context.Background(), root, scope, key, &out)
			if err == nil || out.Len() != 0 {
				t.Fatalf("err=%v stdout=%q", err, out.String())
			}
		})
	}
}

func TestAuditAcceptanceMismatchesWriteNoStdout(t *testing.T) {
	f := newProductionFixture(t)
	if _, err := f.run(t, f.request); err != nil {
		t.Fatal(err)
	}
	reg, err := accept.Open(f.acceptRoot)
	if err != nil {
		t.Fatal(err)
	}
	session := reg.SessionPath(f.request.Scope, f.request.IdempotencyKey)
	if err := os.Remove(session); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = auditAcceptance(context.Background(), f.acceptRoot, f.request.Scope, f.request.IdempotencyKey, &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("session_missing: err=%v stdout=%q", err, out.String())
	}
	var missing bytes.Buffer
	err = auditAcceptance(context.Background(), f.acceptRoot, f.request.Scope, "missing-key", &missing)
	if err == nil || missing.Len() != 0 {
		t.Fatalf("claim_missing: err=%v stdout=%q", err, missing.String())
	}
}
