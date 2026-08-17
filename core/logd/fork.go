package logd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Fork는 원본 세션을 atSeq 지점(포함)까지 새 세션으로 포크한다 (FR-LOG-05).
//
//   - 원본(src)은 읽기만 한다 — 원본 로그는 불변이다.
//   - 포크 세션의 첫 이벤트는 session/fork이며 원본 참조
//     (origin_trace_id, origin_seq)를 보존한다.
//   - 이어서 원본의 1..atSeq 이벤트가 newTraceID로 복사되고, seq는
//     대상 writer가 새로 발급한다(span 구조와 ts는 보존).
//   - 목적지는 반드시 비어 있어야 하며, 공백 확인과 배치 기록은 대상
//     writer 루프 안에서 원자적으로 직렬화된다(TOCTOU 없음). 같은 Log의
//     Reader/Writer를 넘기는 자기 포크는 원본이 비어 있지 않으므로
//     ErrDestinationNotEmpty로 거부된다 — 원본은 어떤 경로로도 오염되지
//     않는다.
//
// 복사도 dst writer의 파이프라인을 경유하므로 redaction·contracts 검증·
// 단일 writer 불변식이 그대로 적용된다.
func Fork(ctx context.Context, src Reader, atSeq int64, newTraceID string, dst *Writer) error {
	if atSeq < 1 {
		return fmt.Errorf("logd: 포크 지점 seq %d — 1 이상이어야 함", atSeq)
	}
	events, err := src.ReadFrom(ctx, 1)
	if err != nil {
		return fmt.Errorf("logd: 원본 읽기: %w", err)
	}
	if len(events) == 0 {
		return fmt.Errorf("logd: 빈 세션은 포크할 수 없다")
	}
	origin := events[0].TraceID
	if newTraceID == origin {
		return fmt.Errorf("logd: 포크 세션은 새 trace_id를 가져야 한다 (원본과 동일: %s)", newTraceID)
	}

	// atSeq는 원본에 실제로 존재하는 seq여야 한다 — seq gap 너머나
	// 첫 이벤트 이전 지점으로의 포크는 존재하지 않는 상태의 포크다.
	var cut []gen.EventRecord
	found := false
	for _, e := range events {
		if e.Seq <= atSeq {
			cut = append(cut, e)
		}
		if e.Seq == atSeq {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("logd: 포크 지점 seq %d가 원본에 존재하지 않음 (범위 %d..%d)",
			atSeq, events[0].Seq, events[len(events)-1].Seq)
	}

	forkPayload, err := json.Marshal(gen.SessionForkPayload{
		OriginTraceID: origin,
		OriginSeq:     atSeq,
	})
	if err != nil {
		return err
	}
	batch := make([]gen.EventRecord, 0, len(cut)+1)
	batch = append(batch, gen.EventRecord{
		Ts:      cut[len(cut)-1].Ts, // 포크 지점 이벤트의 시각 — 재현 가능성 유지
		TraceID: newTraceID,
		SpanID:  cut[0].SpanID,
		Kind:    gen.KindSessionFork,
		Actor:   "parent",
		Payload: forkPayload,
	})
	for _, e := range cut {
		e.TraceID = newTraceID
		batch = append(batch, e)
	}
	if err := dst.InitBatch(ctx, batch); err != nil {
		return fmt.Errorf("logd: 포크 배치 기록: %w", err)
	}
	return nil
}
