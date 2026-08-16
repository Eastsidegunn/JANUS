package sqlite_test

// T5 실파일 E2E: reject된 첫 step의 "step 없는 durable turn"(FR-LOOP-05)이
// 실제 SQLite 로그에 내구 기록되고 reopen 후에도 재생 가능함을 확인한다.
// (core는 seams를 import할 수 없으므로 이 통합 테스트는 seam 쪽에 둔다.)

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"github.com/Eastsidegunn/JANUS/core/loop"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

type nullModel struct{}

func (nullModel) Complete(ctx context.Context, req loop.ModelRequest) (loop.ModelResponse, error) {
	return loop.ModelResponse{Text: "호출되면 안 됨"}, nil
}

type nullTools struct{}

func (nullTools) Invoke(ctx context.Context, call loop.ToolCall) (loop.ToolResult, error) {
	return loop.ToolResult{Output: json.RawMessage(`null`)}, nil
}

func TestRejectedTurnDurableOverSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loop.db")
	l, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	var tick int64
	lp := loop.New(l.Writer, l.Reader, nullModel{}, nullTools{},
		strings.Repeat("a", 32), strings.Repeat("b", 16),
		loop.WithClock(func() int64 { tick++; return tick }))
	if err := lp.RegisterHook(gen.HookPointPreStep, func(ctx context.Context, hc loop.HookContext) loop.Decision {
		return loop.Reject("정책 거부")
	}); err != nil {
		t.Fatal(err)
	}
	if err := lp.RunTurn(ctx, "시도"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen 후에도 시도의 기록이 남아 있고 재생 가능하다
	l2, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	events, err := l2.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []gen.Kind{gen.KindTurnStart, gen.KindUserMessage, gen.KindHookVerdict, gen.KindTurnEnd}
	if len(events) != len(want) {
		t.Fatalf("이벤트 %d건 (%d건 기대)", len(events), len(want))
	}
	for i, k := range want {
		if events[i].Kind != k {
			t.Fatalf("이벤트 %d: %s (%s 기대)", i, events[i].Kind, k)
		}
	}
	state, err := logd.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if state.Turns != 1 || state.Steps != 0 {
		t.Fatalf("turns=%d steps=%d (1/0 기대) — step 없는 turn이 아님", state.Turns, state.Steps)
	}
}
