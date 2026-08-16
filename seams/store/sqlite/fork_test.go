package sqlite_test

// FR-LOG-05 실파일 E2E: SQLite 세션 파일을 포크한 뒤 두 세션이 독립적으로
// 진행되고 원본 로그가 불변임을 공개 API(Log)만으로 확인한다 (§8-6의 전신).

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func evt(kind gen.Kind, payload string) gen.EventRecord {
	return gen.EventRecord{
		Ts: 1, TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16),
		Kind: kind, Actor: "parent", Payload: []byte(payload),
	}
}

func TestForkAcrossFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	orig, err := sqlite.Open(ctx, filepath.Join(dir, "orig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	for _, e := range []gen.EventRecord{
		evt(gen.KindSessionStart, `{}`),
		evt(gen.KindUserMessage, `{"text":"하나"}`),
		evt(gen.KindAssistantMessage, `{"text":"둘"}`),
		evt(gen.KindUserMessage, `{"text":"셋"}`),
	} {
		if _, err := orig.Writer.Submit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	before, err := orig.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	fork, err := sqlite.Open(ctx, filepath.Join(dir, "fork.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	newTrace := strings.Repeat("f", 32)
	if err := logd.Fork(ctx, orig.Reader, 3, newTrace, fork.Writer); err != nil {
		t.Fatal(err)
	}

	// 두 세션이 독립적으로 진행된다
	if _, err := fork.Writer.Submit(ctx, gen.EventRecord{
		Ts: 9, TraceID: newTrace, SpanID: strings.Repeat("b", 16),
		Kind: gen.KindUserMessage, Actor: "parent", Payload: []byte(`{"text":"포크 이후"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := orig.Writer.Submit(ctx, evt(gen.KindSessionEnd, `{}`)); err != nil {
		t.Fatal(err)
	}

	// 원본 불변: 포크 이전 구간(1..4)이 그대로다
	after, err := orig.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("포크·독립 진행 후 원본 로그의 기존 구간이 변형됨")
	}

	fs, err := logd.ReplayReader(ctx, fork.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if fs.TraceID != newTrace || fs.Origin == nil || fs.Origin.OriginSeq != 3 {
		t.Fatalf("포크 세션 상태 이상: trace=%s origin=%+v", fs.TraceID, fs.Origin)
	}
	if got := len(fs.Messages); got != 3 { // 하나·둘 + 포크 이후
		t.Fatalf("포크 세션 히스토리 %d항목 (3 기대): %+v", got, fs.Messages)
	}
	osR, err := logd.ReplayReader(ctx, orig.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if osR.Origin != nil || !osR.Ended {
		t.Fatalf("원본 세션 상태 이상: %+v", osR)
	}
}
