package core

// FR-LOG-06 리플레이 결정론 속성 테스트.
// T2에서 xfail(구현 전 실패 기대)로 커밋됐고, T4에서 core/logd.Replay가
// 배선되며 본 스위트로 편입됐다. 반복 횟수 축소는 금지(CLAUDE.md).

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
)

const replayPropIterations = 300

// replayFn은 동일 이벤트 시퀀스로부터 파생 상태를 재계산한다.
// T4에서 core/logd.Replay로 배선됐다.
var replayFn = func(events []gen.EventRecord) (derived any, err error) {
	return logd.Replay(events)
}

func TestPropertyReplayDeterminism(t *testing.T) {
	if replayFn == nil {
		t.Fatal("FR-LOG-06: replay 구현이 아직 배선되지 않음 — T4에서 replayFn 배선 후 xfail 태그 제거")
	}
	for seed := 0; seed < replayPropIterations; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		events := genEventSequence(r)

		s1, err1 := replayFn(events)
		s2, err2 := replayFn(events)
		if err1 != nil || err2 != nil {
			t.Fatalf("seed %d: 유효 시퀀스 재생이 실패 (err1=%v, err2=%v)", seed, err1, err2)
		}
		if !reflect.DeepEqual(s1, s2) {
			t.Fatalf("seed %d: 동일 시퀀스의 재생이 다른 파생 상태를 산출\n1회차=%v\n2회차=%v", seed, s1, s2)
		}

		// 입력 불변성: 재생이 이벤트 시퀀스 자체를 변형하면 안 된다(append-only).
		again := genEventSequence(rand.New(rand.NewSource(int64(seed))))
		if !reflect.DeepEqual(events, again) {
			t.Fatalf("seed %d: replay가 입력 이벤트를 변형함", seed)
		}
	}
}
