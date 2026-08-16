package logd

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// ErrClosed는 닫힌 writer에 대한 Submit이다.
var ErrClosed = errors.New("logd: writer가 닫힘")

// DefaultQueueCap은 writer bounded queue의 기본 용량이다.
// 큐 포화 시 Submit이 블록되는 것이 FR-LOG-09의 백프레셔이며,
// 호출자(어댑터 스트림 펌프)의 입력 일시정지로 전파된다.
const DefaultQueueCap = 1024

// Writer는 세션 로그의 유일한 쓰기 진입점이다 (FR-LOG-02).
// 단일 goroutine이 큐를 소비하며 seq를 발급하고, redaction 패스(FR-LOG-08)
// 후 Store에 내구 커밋한다. 수락(큐 진입)된 이벤트는 유실되지 않는다 —
// 커밋 실패 시에도 해당 제출자에게 오류로 반환될 뿐 조용히 버려지지 않는다.
type Writer struct {
	store    Store
	redactor *Redactor
	queue    chan submission

	closeOnce sync.Once
	closing   chan struct{} // Submit 차단용
	drained   chan struct{} // 루프 종료 신호
}

type submission struct {
	rec gen.EventRecord
	ack chan ackResult
}

type ackResult struct {
	seq int64
	err error
}

type writerConfig struct {
	queueCap       int
	extraRedaction []string
}

// Option은 NewWriter의 설정이다.
type Option func(*writerConfig)

// WithQueueCap은 bounded queue 용량을 바꾼다(백프레셔 테스트·튜닝용).
func WithQueueCap(n int) Option {
	return func(c *writerConfig) { c.queueCap = n }
}

// WithRedactionPatterns는 기본 redaction 패턴에 정규식을 추가한다(FR-LOG-08).
func WithRedactionPatterns(patterns ...string) Option {
	return func(c *writerConfig) { c.extraRedaction = append(c.extraRedaction, patterns...) }
}

// NewWriter는 store의 마지막 seq에 이어서 발급을 시작하는 writer를 만든다.
func NewWriter(ctx context.Context, store Store, opts ...Option) (*Writer, error) {
	cfg := writerConfig{queueCap: DefaultQueueCap}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.queueCap < 1 {
		return nil, fmt.Errorf("logd: queue 용량은 1 이상이어야 함 (%d)", cfg.queueCap)
	}
	red, err := NewRedactor(cfg.extraRedaction...)
	if err != nil {
		return nil, err
	}
	last, err := store.LastSeq(ctx)
	if err != nil {
		return nil, fmt.Errorf("logd: 마지막 seq 조회: %w", err)
	}
	w := &Writer{
		store:    store,
		redactor: red,
		queue:    make(chan submission, cfg.queueCap),
		closing:  make(chan struct{}),
		drained:  make(chan struct{}),
	}
	go w.loop(last + 1)
	return w, nil
}

// Submit은 이벤트를 로그에 기록한다. rec.Seq는 무시되고 writer가 발급한다.
// 반환된 seq는 내구 커밋이 끝난(acknowledge된) 값이다.
//
// 큐가 포화면 공간이 생길 때까지 블록된다 — FR-LOG-09 백프레셔.
// ctx 취소로 커밋 전에 반환되더라도 이미 큐에 수락된 이벤트는 유실되지 않고
// 기록된다(호출자가 ack만 놓친다).
func (w *Writer) Submit(ctx context.Context, rec gen.EventRecord) (int64, error) {
	select {
	case <-w.closing:
		return 0, ErrClosed
	default:
	}
	sub := submission{rec: rec, ack: make(chan ackResult, 1)}
	select {
	case w.queue <- sub:
	case <-w.closing:
		return 0, ErrClosed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case r := <-sub.ack:
		return r.seq, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-w.drained:
		// 종료 드레인 직후 경합 — 처리됐을 수 있으니 non-blocking 재확인
		select {
		case r := <-sub.ack:
			return r.seq, r.err
		default:
			return 0, ErrClosed
		}
	}
}

// Close는 새 Submit을 차단하고, 이미 수락된 큐를 전부 커밋한 뒤 반환한다.
// store의 Close는 소유자(조립 지점)가 별도로 한다.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() { close(w.closing) })
	<-w.drained
	return nil
}

func (w *Writer) loop(nextSeq int64) {
	defer close(w.drained)
	process := func(sub submission) {
		rec := sub.rec
		rec.Seq = nextSeq
		if err := w.redactor.RedactEvent(&rec); err != nil {
			sub.ack <- ackResult{err: fmt.Errorf("logd: redaction: %w", err)}
			return
		}
		// 커밋은 제출자의 ctx와 무관하게 완주한다 — 수락된 이벤트 유실 0.
		if err := w.store.Append(context.Background(), rec); err != nil {
			sub.ack <- ackResult{err: fmt.Errorf("logd: append seq %d: %w", rec.Seq, err)}
			return
		}
		nextSeq++
		sub.ack <- ackResult{seq: rec.Seq}
	}
	for {
		select {
		case sub := <-w.queue:
			process(sub)
		case <-w.closing:
			// 이미 수락된 제출을 전부 커밋하고 종료
			for {
				select {
				case sub := <-w.queue:
					process(sub)
				default:
					return
				}
			}
		}
	}
}
