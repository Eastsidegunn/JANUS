package core

// FR-POL-03 병합 협소성 속성 테스트.
// T2에서 xfail(구현 전 실패 기대)로 커밋됐고, T6에서 core/policy.Merge가
// 배선되며 본 스위트로 편입됐다. 반복 횟수 축소는 금지(CLAUDE.md).

import (
	"math/rand"
	"testing"

	"github.com/Eastsidegunn/JANUS/core/policy"
)

const mergePropIterations = 300

// mergeProfilesFn은 프로파일 오버레이 병합이다. T6에서 배선됐다.
// 불변식: 병합은 강화만 — allow 리스트는 교집합, 예산은 최솟값,
// 어떤 조합도 상위 프로파일보다 넓은 권한을 만들 수 없다.
var mergeProfilesFn = policy.Merge

func TestPropertyProfileMergeOnlyNarrows(t *testing.T) {
	if mergeProfilesFn == nil {
		t.Fatal("FR-POL-03: 프로파일 병합 구현이 아직 배선되지 않음 — T6에서 mergeProfilesFn 배선 후 xfail 태그 제거")
	}
	for seed := 0; seed < mergePropIterations; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		a, b, c := genProfile(r), genProfile(r), genProfile(r)

		m := mergeProfilesFn(a, b)
		assertNarrower(t, seed, "merge(a,b)⊑a", m, a)
		assertNarrower(t, seed, "merge(a,b)⊑b", m, b)

		// 오버레이 체인도 계속 좁아지기만 한다
		m2 := mergeProfilesFn(m, c)
		assertNarrower(t, seed, "merge(merge(a,b),c)⊑merge(a,b)", m2, m)
		assertNarrower(t, seed, "merge(merge(a,b),c)⊑c", m2, c)

		// 자기 자신과의 병합이 권한을 넓히면 안 된다
		assertNarrower(t, seed, "merge(a,a)⊑a", mergeProfilesFn(a, a), a)

		// 교환해도 넓어질 수 없다
		assertNarrower(t, seed, "merge(b,a)⊑a", mergeProfilesFn(b, a), a)
	}
}

// assertNarrower는 narrow가 wide보다 어떤 축에서도 넓지 않음을 검사한다.
func assertNarrower(t *testing.T, seed int, label string, narrow, wide Profile) {
	t.Helper()
	if !subset(narrow.Egress, wide.Egress) {
		t.Fatalf("seed %d %s: egress가 넓어짐 (%v ⊄ %v)", seed, label, narrow.Egress, wide.Egress)
	}
	if !subset(narrow.FSScope, wide.FSScope) {
		t.Fatalf("seed %d %s: fs 스코프가 넓어짐 (%v ⊄ %v)", seed, label, narrow.FSScope, wide.FSScope)
	}
	if narrow.Budget.Tokens > wide.Budget.Tokens ||
		narrow.Budget.TimeMs > wide.Budget.TimeMs ||
		narrow.Budget.MaxDepth > wide.Budget.MaxDepth {
		t.Fatalf("seed %d %s: 예산이 커짐 (%+v > %+v)", seed, label, narrow.Budget, wide.Budget)
	}
	// 자동 승인은 양쪽 모두 auto일 때만 유지될 수 있다 — narrow가 auto인데
	// wide가 manual이면 오버레이가 승인 게이트를 풀어버린 것이다.
	if narrow.Approval == policy.ApprovalAuto && wide.Approval != policy.ApprovalAuto {
		t.Fatalf("seed %d %s: 승인 모드가 완화됨 (manual → auto)", seed, label)
	}
}

func subset(sub, super []string) bool {
	set := map[string]bool{}
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}
