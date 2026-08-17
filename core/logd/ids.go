package logd

import (
	"crypto/rand"
	"encoding/hex"
)

// NewTraceID는 OTel 호환 trace_id(32자리 소문자 hex, all-zero 불가)를
// 발급한다 (FR-OBS-01). 스키마의 all-zero 거부 패턴과 정합.
func NewTraceID() string { return randomHexID(16) }

// NewSpanID는 OTel 호환 span_id(16자리 소문자 hex, all-zero 불가)를 발급한다.
func NewSpanID() string { return randomHexID(8) }

func randomHexID(bytes int) string {
	b := make([]byte, bytes)
	for {
		if _, err := rand.Read(b); err != nil {
			panic("logd: 난수원 실패: " + err.Error()) // crypto/rand 실패는 환경 손상
		}
		for _, c := range b {
			if c != 0 {
				return hex.EncodeToString(b)
			}
		}
		// all-zero면 재추첨 (OTel 규격상 무효)
	}
}
