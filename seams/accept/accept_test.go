package accept

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func acceptance(fp string) Acceptance {
	return Acceptance{
		Scope: "rhizome-test/executor-a", Key: "key-1", Fingerprint: fp,
		TraceID: "0123456789abcdef0123456789abcdef", PolicyHash: "p", OperationID: "op-1",
		CreatedAtMs: 1,
	}
}

func TestClaimExclusiveThenIdempotent(t *testing.T) {
	r := newRegistry(t)
	first, created, err := r.Claim(acceptance("fp-a"))
	if err != nil || !created {
		t.Fatalf("최초 claim: created=%v err=%v", created, err)
	}
	// 같은 key+fingerprint: 기존 claim 반환, 새 점유 아님.
	dup := acceptance("fp-a")
	dup.TraceID = "ffffffffffffffffffffffffffffffff" // 재시도가 새 trace를 제안해도
	got, created, err := r.Claim(dup)
	if err != nil || created {
		t.Fatalf("중복 claim: created=%v err=%v", created, err)
	}
	if got.TraceID != first.TraceID {
		t.Fatalf("중복 claim이 기존 binding을 반환해야 함: got=%s want=%s", got.TraceID, first.TraceID)
	}
	// 같은 key+다른 fingerprint: KEY_CONFLICT.
	if _, _, err := r.Claim(acceptance("fp-B")); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("fingerprint 불일치는 ErrKeyConflict여야 함: %v", err)
	}
}

func TestClaimConcurrentSingleWinner(t *testing.T) {
	r := newRegistry(t)
	const n = 16
	var wg sync.WaitGroup
	winners := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acc := acceptance("fp-a")
			acc.TraceID = fmt.Sprintf("%032d", i)
			got, created, err := r.Claim(acc)
			if err != nil {
				t.Errorf("동시 claim %d: %v", i, err)
				return
			}
			if created {
				winners <- got.TraceID
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	var won []string
	for w := range winners {
		won = append(won, w)
	}
	if len(won) != 1 {
		t.Fatalf("배타 점유 승자는 정확히 1이어야 함: %d", len(won))
	}
	status, err := r.Lookup("rhizome-test/executor-a", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.Acceptance.TraceID != won[0] {
		t.Fatalf("durable claim이 승자의 binding이어야 함: %s != %s", status.Acceptance.TraceID, won[0])
	}
}

func TestLaunchClaimSingle(t *testing.T) {
	r := newRegistry(t)
	if _, _, err := r.Claim(acceptance("fp-a")); err != nil {
		t.Fatal(err)
	}
	scope, key := "rhizome-test/executor-a", "key-1"
	if err := r.MarkDBInitialized(scope, key); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDBInitialized(scope, key); err != nil {
		t.Fatalf("db-initialized는 멱등이어야 함: %v", err)
	}
	if err := r.MarkLaunched(scope, key); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkLaunched(scope, key); !errors.Is(err, ErrAlreadyLaunched) {
		t.Fatalf("두 번째 launch claim은 ErrAlreadyLaunched여야 함: %v", err)
	}
}

func TestMarkWithoutClaimRejected(t *testing.T) {
	r := newRegistry(t)
	if err := r.MarkLaunched("s", "k"); err == nil {
		t.Fatal("claim 없는 launch 마커는 거부돼야 함")
	}
}

func TestLookupStates(t *testing.T) {
	r := newRegistry(t)
	scope, key := "rhizome-test/executor-a", "key-1"
	status, err := r.Lookup(scope, key)
	if err != nil || status.State != "not_submitted" {
		t.Fatalf("미제출 상태: %+v err=%v", status, err)
	}
	if _, _, err := r.Claim(acceptance("fp-a")); err != nil {
		t.Fatal(err)
	}
	status, _ = r.Lookup(scope, key)
	if status.State != "initializing" || status.Launched {
		t.Fatalf("claim 직후는 initializing이어야 함: %+v", status)
	}
	if err := r.MarkDBInitialized(scope, key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.SessionPath(scope, key), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkLaunched(scope, key); err != nil {
		t.Fatal(err)
	}
	status, _ = r.Lookup(scope, key)
	if status.State != "accepted" || !status.Launched || !status.SessionExists {
		t.Fatalf("전체 진행 후 상태: %+v", status)
	}
	// tombstone: 세션 파일이 삭제돼도 claim은 남는다 — 재점유 불가.
	if err := os.Remove(r.SessionPath(scope, key)); err != nil {
		t.Fatal(err)
	}
	status, _ = r.Lookup(scope, key)
	if status.SessionExists {
		t.Fatal("세션 파일 삭제가 관측돼야 함")
	}
	if _, created, err := r.Claim(acceptance("fp-a")); err != nil || created {
		t.Fatalf("삭제 후에도 claim은 기존 binding 반환이어야 함: created=%v err=%v", created, err)
	}
}

func TestKeyHashLengthPrefixDisambiguation(t *testing.T) {
	if KeyHash("ab", "c") == KeyHash("a", "bc") {
		t.Fatal("길이 접두사가 scope/key 경계를 구분해야 함")
	}
	if KeyHash("s", "k") != KeyHash("s", "k") {
		t.Fatal("KeyHash는 결정론적이어야 함")
	}
}

func TestClaimCorruptDetected(t *testing.T) {
	r := newRegistry(t)
	if _, _, err := r.Claim(acceptance("fp-a")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(r.SessionPath("rhizome-test/executor-a", "key-1"))
	// claim은 불변이지만, 외부 손상(비트 부패 등)은 자동 복구 없이 식별돼야 한다.
	if err := os.Remove(filepath.Join(dir, "claim.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claim.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("rhizome-test/executor-a", "key-1"); !errors.Is(err, ErrClaimCorrupt) {
		t.Fatalf("손상 claim은 ErrClaimCorrupt여야 함: %v", err)
	}
}
