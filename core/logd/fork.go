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
//
// 복사도 dst writer를 경유하므로 redaction·contracts 검증·단일 writer
// 불변식이 그대로 적용된다.
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
	if last := events[len(events)-1].Seq; atSeq > last {
		return fmt.Errorf("logd: 포크 지점 seq %d — 원본 마지막 seq %d 초과", atSeq, last)
	}
	origin := events[0].TraceID
	if newTraceID == origin {
		return fmt.Errorf("logd: 포크 세션은 새 trace_id를 가져야 한다 (원본과 동일: %s)", newTraceID)
	}

	var cut []gen.EventRecord
	for _, e := range events {
		if e.Seq <= atSeq {
			cut = append(cut, e)
		}
	}

	forkPayload, err := json.Marshal(gen.SessionForkPayload{
		OriginTraceID: origin,
		OriginSeq:     atSeq,
	})
	if err != nil {
		return err
	}
	forkEvent := gen.EventRecord{
		Ts:      cut[len(cut)-1].Ts, // 포크 지점 이벤트의 시각 — 재현 가능성 유지
		TraceID: newTraceID,
		SpanID:  cut[0].SpanID,
		Kind:    gen.KindSessionFork,
		Actor:   "parent",
		Payload: forkPayload,
	}
	if _, err := dst.Submit(ctx, forkEvent); err != nil {
		return fmt.Errorf("logd: session/fork 기록: %w", err)
	}
	for _, e := range cut {
		e.TraceID = newTraceID
		if _, err := dst.Submit(ctx, e); err != nil {
			return fmt.Errorf("logd: 원본 seq %d 복사: %w", e.Seq, err)
		}
	}
	return nil
}
