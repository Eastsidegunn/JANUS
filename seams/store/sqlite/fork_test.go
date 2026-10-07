package sqlite_test

// FR-LOG-05 실파일 E2E: SQLite 세션 파일을 포크한 뒤 두 세션이 독립적으로
// 진행되고 원본 로그가 불변임을 공개 API(Log)만으로 확인한다 (§8-6의 전신).

import (
	"context"
	"errors"
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

// T4 재리뷰 차단 1의 실파일 회귀: 공개 API로 자기 자신·비공백 목적지 포크를
// 시도해도 거부되고 양쪽 로그가 완전히 불변이다.
func TestForkDestinationSafetyE2E(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	src, err := sqlite.Open(ctx, filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, e := range []gen.EventRecord{
		evt(gen.KindSessionStart, `{}`),
		evt(gen.KindUserMessage, `{"text":"원본"}`),
	} {
		if _, err := src.Writer.Submit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	srcBefore, err := src.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	newTrace := strings.Repeat("f", 32)

	// 리뷰 실증 그대로: 같은 Log의 Reader/Writer를 목적지로 전달
	if err := logd.Fork(ctx, src.Reader, 1, newTrace, src.Writer); !errors.Is(err, logd.ErrDestinationNotEmpty) {
		t.Fatalf("자기 포크 = %v (ErrDestinationNotEmpty 기대)", err)
	}
	srcAfter, err := src.Reader.ReadFrom(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(srcBefore, srcAfter) {
		t.Fatal("자기 포크 거부 후에도 원본이 변형됨")
	}
	if _, err := logd.ReplayReader(ctx, src.Reader); err != nil {
		t.Fatalf("자기 포크 시도 후 원본 재생 불가: %v", err)
	}

	// 비어 있지 않은 별도 목적지
	dst, err := sqlite.Open(ctx, filepath.Join(dir, "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.Writer.Submit(ctx, evt(gen.KindSessionStart, `{}`)); err != nil {
		t.Fatal(err)
	}
	dstBefore, _ := dst.Reader.ReadFrom(ctx, 1)
	if err := logd.Fork(ctx, src.Reader, 1, newTrace, dst.Writer); !errors.Is(err, logd.ErrDestinationNotEmpty) {
		t.Fatalf("비공백 목적지 포크 = %v (ErrDestinationNotEmpty 기대)", err)
	}
	dstAfter, _ := dst.Reader.ReadFrom(ctx, 1)
	if !reflect.DeepEqual(dstBefore, dstAfter) {
		t.Fatal("거부된 포크가 목적지를 변형함")
	}
	srcFinal, _ := src.Reader.ReadFrom(ctx, 1)
	if !reflect.DeepEqual(srcBefore, srcFinal) {
		t.Fatal("비공백 목적지 포크 시도 후 원본이 변형됨")
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
