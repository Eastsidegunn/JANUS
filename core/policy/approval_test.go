package policy

import (
	"context"
	"testing"
)

func TestDenyAll(t *testing.T) {
	got, err := (DenyAll{}).Decide(context.Background(), ApprovalRequest{RequestID: "r"})
	if err != nil || got.Allow || got.Reason == "" {
		t.Fatalf("DenyAll = %+v, %v", got, err)
	}
}
