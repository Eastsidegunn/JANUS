package logd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
)

// ErrClosed는 닫힌 writer에 대한 Submit이다.
var ErrClosed = errors.New("logd: writer가 닫힘")

// ErrDestinationNotEmpty는 배타적 초기화 배치(포크)의 목적지에 이미
// 이벤트가 있음을 뜻한다 — 포크는 빈 로그만 독점적으로 초기화할 수 있다.
var ErrDestinationNotEmpty = errors.New("logd: 포크 목적지가 비어 있지 않음")

// ErrTerminal은 store 커밋 실패로 writer가 종료 상태에 들어갔음을 뜻한다.
// 종료 상태에서는 더 이상 어떤 이벤트도 커밋되지 않으며, 이후의 Submit과
// Close가 모두 이 오류(원인 포함)를 반환한다 — 조용한 진행은 유실 위험이다.
var ErrTerminal = errors.New("logd: writer 종료 상태 (store 커밋 실패)")

// DefaultQueueCap은 writer bounded queue의 기본 용량이다.
// 큐 포화 시 Submit이 블록되는 것이 FR-LOG-09의 백프레셔이며,
// 호출자(어댑터 스트림 펌프)의 입력 일시정지로 전파된다.
const DefaultQueueCap = 1024

// Writer는 세션 로그의 유일한 쓰기 진입점이다 (FR-LOG-02).
// 단일 goroutine이 큐를 소비하며 seq를 발급하고, redaction 패스(FR-LOG-08)와
// contracts 스키마 검증을 거친 뒤에만 Store에 내구 커밋한다.
//
// ack 계약: 호출자 context는 큐 admission까지만 적용된다. 큐에 수락된
// 제출은 반드시 커밋 결과(성공 seq 또는 오류)를 ack로 돌려받는다 —
// "커밋됐는지 모르는" 상태가 없으므로 호출자의 중복 재시도가 원천 차단된다.
type Writer struct {
	store      Store
	redactor   *Redactor
	validators *validate.Validators
	queue      chan submission

	closeOnce sync.Once
	closing   chan struct{} // Submit 차단용
	drained   chan struct{} // 루프 종료 신호

	mu          sync.Mutex
	terminalErr error
}

type submission struct {
	rec gen.EventRecord
	// batch가 non-nil이면 배타적 초기화 배치다(포크 전용): 루프가
	// "로그가 비어 있음"의 확인과 배치 기록을 원자적으로 수행한다.
	// 루프가 유일한 seq 발급자이므로 동시 Submit과의 TOCTOU가 없다.
	batch []gen.EventRecord
	ack   chan ackResult
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
	vals, err := validate.New()
	if err != nil {
		return nil, fmt.Errorf("logd: 스키마 검증기: %w", err)
	}
	last, err := store.LastSeq(ctx)
	if err != nil {
		return nil, fmt.Errorf("logd: 마지막 seq 조회: %w", err)
	}
	w := &Writer{
		store:      store,
		redactor:   red,
		validators: vals,
		queue:      make(chan submission, cfg.queueCap),
		closing:    make(chan struct{}),
		drained:    make(chan struct{}),
	}
	go w.loop(last + 1)
	return w, nil
}

// Submit은 이벤트를 로그에 기록한다. rec.Seq는 무시되고 writer가 발급한다.
// 반환된 seq는 내구 커밋이 끝난(acknowledge된) 값이다.
//
// 큐가 포화면 공간이 생길 때까지 블록된다 — FR-LOG-09 백프레셔.
// ctx는 admission까지만 적용된다: 큐에 수락된 뒤에는 취소와 무관하게
// 커밋 결과를 기다려 반환한다(커밋 여부가 모호한 반환은 없다).
func (w *Writer) Submit(ctx context.Context, rec gen.EventRecord) (int64, error) {
	if err := w.terminal(); err != nil {
		return 0, err
	}
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
	return w.awaitAck(sub)
}

// awaitAck는 admission된 제출의 ack를 기다린다 — 루프는 Close 전까지
// 살아 있고 Close는 수락분을 전부 드레인하므로 ack는 반드시 온다.
func (w *Writer) awaitAck(sub submission) (int64, error) {
	select {
	case r := <-sub.ack:
		return r.seq, r.err
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

// submitInitBatch는 빈 로그에만 허용되는 배타적 초기화 배치다(포크 경로).
// 공백 확인과 배치 기록이 writer 루프 안에서 직렬화되므로, 목적지에
// 이벤트가 하나라도 커밋됐거나 배치보다 먼저 admission된 제출이 처리되면
// ErrDestinationNotEmpty로 거부된다. 배치는 전건 사전 검증(redaction·
// contracts) 후에만 기록을 시작한다 — 검증 실패 시 목적지는 계속 비어 있다.
func (w *Writer) submitInitBatch(ctx context.Context, events []gen.EventRecord) error {
	if len(events) == 0 {
		return fmt.Errorf("logd: 빈 초기화 배치")
	}
	if err := w.terminal(); err != nil {
		return err
	}
	select {
	case <-w.closing:
		return ErrClosed
	default:
	}
	sub := submission{batch: events, ack: make(chan ackResult, 1)}
	select {
	case w.queue <- sub:
	case <-w.closing:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := w.awaitAck(sub)
	return err
}

// Close는 새 Submit을 차단하고, 이미 수락된 큐를 전부 처리한 뒤 반환한다.
// writer가 종료 상태면 그 원인을 반환한다. store의 Close는 소유자
// (조립 지점)가 별도로 한다.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() { close(w.closing) })
	<-w.drained
	return w.terminal()
}

func (w *Writer) terminal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminalErr == nil {
		return nil
	}
	return &terminalError{cause: w.terminalErr}
}

// terminalError는 ErrTerminal과 원인 양쪽으로 errors.Is/As 체인을 유지한다.
type terminalError struct {
	cause error
}

func (e *terminalError) Error() string {
	return ErrTerminal.Error() + ": " + e.cause.Error()
}

func (e *terminalError) Unwrap() []error {
	return []error{ErrTerminal, e.cause}
}

func (w *Writer) setTerminal(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminalErr == nil {
		w.terminalErr = err
	}
}

func (w *Writer) loop(nextSeq int64) {
	defer close(w.drained)
	// prepare는 기록 전 파이프라인이다: redaction 패스(FR-LOG-08) 후
	// contracts 스키마 검증 — 위반 이벤트는 로그에 들어갈 수 없다
	// (T1 계약의 런타임 강제).
	prepare := func(rec gen.EventRecord, seq int64) (gen.EventRecord, error) {
		rec.Seq = seq
		if err := w.redactor.RedactEvent(&rec); err != nil {
			return rec, fmt.Errorf("logd: 이벤트 거부 (redaction): %w", err)
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return rec, fmt.Errorf("logd: 이벤트 거부 (직렬화 불가): %w", err)
		}
		if err := w.validators.ValidateRecord(encoded); err != nil {
			return rec, fmt.Errorf("logd: 이벤트 거부 (contracts 위반): %w", err)
		}
		return rec, nil
	}
	// append는 내구 커밋이다. 실패는 회복 불능 — writer 종료 상태로 승격.
	append_ := func(rec gen.EventRecord) error {
		if err := w.store.Append(context.Background(), rec); err != nil {
			w.setTerminal(fmt.Errorf("seq %d append: %w", rec.Seq, err))
			return w.terminal()
		}
		return nil
	}
	process := func(sub submission) {
		if err := w.terminal(); err != nil {
			sub.ack <- ackResult{err: err}
			return
		}
		if sub.batch != nil {
			// 배타적 초기화 배치(포크): 빈 로그(nextSeq==1)에서만 허용.
			// 이 확인은 루프 안이므로 동시 Submit과 원자적으로 직렬화된다.
			if nextSeq != 1 {
				sub.ack <- ackResult{err: ErrDestinationNotEmpty}
				return
			}
			prepared := make([]gen.EventRecord, 0, len(sub.batch))
			for i, rec := range sub.batch {
				p, err := prepare(rec, int64(i+1))
				if err != nil {
					// 전건 사전 검증 실패 — 아무것도 쓰지 않았으므로
					// 목적지는 계속 비어 있다.
					sub.ack <- ackResult{err: fmt.Errorf("logd: 배치 %d번째: %w", i+1, err)}
					return
				}
				prepared = append(prepared, p)
			}
			for _, rec := range prepared {
				if err := append_(rec); err != nil {
					sub.ack <- ackResult{err: err}
					return
				}
				nextSeq++
			}
			sub.ack <- ackResult{seq: nextSeq - 1}
			return
		}
		rec, err := prepare(sub.rec, nextSeq)
		if err != nil {
			sub.ack <- ackResult{err: err}
			return
		}
		// 커밋은 제출자의 ctx와 무관하게 완주한다 — 수락된 이벤트에 대해
		// 커밋 여부가 모호한 결과를 만들지 않는다.
		if err := append_(rec); err != nil {
			sub.ack <- ackResult{err: err}
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
			// 이미 수락된 제출을 전부 ack하고 종료
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
