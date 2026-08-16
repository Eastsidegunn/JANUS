// Package logd는 세션 이벤트 로그의 단일 writer를 구현한다 (FR-LOG-02).
// 로그 진입점은 Writer 하나뿐이며, writer가 발급한 seq가 세션 내 이벤트의
// 전순서를 정의한다. 영속화 계약(Store)은 이 패키지가 소유하고 구현은
// seams/store에 있다 (§3.1).
package logd

import (
	"context"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// Store는 append-only 이벤트 로그의 영속화 계약이다.
//
// 구현 규칙:
//   - Append는 내구 커밋(fsync 수준, NFR-02) 후에만 nil을 반환해야 한다 —
//     nil 반환이 곧 acknowledge다.
//   - UPDATE/DELETE 경로를 제공하지 않는다(FR-LOG-01). 저장소 수준에서도
//     물리적으로 차단해야 한다(트리거 등).
//   - 저장소 잠금 경합(SQLITE_BUSY 류)은 구현 내부에서 context-aware
//     재시도로 처리한다. 이것은 FR-LOG-09의 백프레셔와 별개 상태다 —
//     백프레셔는 Writer의 bounded queue 포화에서 발생한다.
type Store interface {
	// LastSeq는 마지막으로 커밋된 seq를 반환한다. 빈 로그면 0.
	LastSeq(ctx context.Context) (int64, error)
	// Append는 rec을 seq 순서대로 영속화한다. rec.Seq는 writer가 발급한다.
	Append(ctx context.Context, rec gen.EventRecord) error
	// ReadFrom은 fromSeq 이상(포함)의 이벤트를 seq 오름차순으로 반환한다.
	// 파생 상태 재계산(FR-LOG-04)과 복구 검증의 읽기 경로다.
	ReadFrom(ctx context.Context, fromSeq int64) ([]gen.EventRecord, error)
	Close() error
}
