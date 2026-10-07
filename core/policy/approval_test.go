package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDenyAll(t *testing.T) {
	got, err := (DenyAll{}).Decide(context.Background(), ApprovalRequest{RequestID: "r"})
	if err != nil || got.Allow || got.Reason == "" {
		t.Fatalf("DenyAll = %+v, %v", got, err)
	}
}

func TestCanonicalArgsV1(t *testing.T) {
	a, err := CanonicalArgs([]byte(`{"n":9007199254740993}`))
	if err != nil || !strings.Contains(string(a), "9007199254740993") {
		t.Fatalf("large integer lost: %s %v", a, err)
	}
	if _, err := CanonicalArgs([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	a, _ = CanonicalArgs([]byte(` {"b":2,"a":1} `))
	b, _ := CanonicalArgs([]byte(`{"a":1,"b":2}`))
	ha := sha256.Sum256(a)
	hb := sha256.Sum256(b)
	if hex.EncodeToString(ha[:]) != hex.EncodeToString(hb[:]) {
		t.Fatalf("canonical digest mismatch: %s %s", a, b)
	}
}
