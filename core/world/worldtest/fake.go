package worldtest

import (
	"context"
	"errors"
	"sync"

	"github.com/Eastsidegunn/JANUS/core/world"
)

var (
	_ world.Backend = (*FakeBackend)(nil)
	_ world.Lease   = (*FakeLease)(nil)
)

// FakeBackend records Open calls and returns a configured FakeLease. It does
// not start a process or sandbox.
type FakeBackend struct {
	mu       sync.Mutex
	lease    *FakeLease
	openErr  error
	openGate <-chan struct{}
	opens    []world.SpawnSpec
}

func NewFakeBackend(lease *FakeLease) *FakeBackend {
	if lease == nil {
		lease = NewFakeLease(world.Endpoint{}, world.SpawnMetadata{}, "", nil)
	}
	return &FakeBackend{lease: lease}
}

func (f *FakeBackend) Open(ctx context.Context, spec world.SpawnSpec) (world.Lease, error) {
	f.mu.Lock()
	f.opens = append(f.opens, spec)
	gate, openErr, lease := f.openGate, f.openErr, f.lease
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if openErr != nil {
		return nil, openErr
	}
	return lease, nil
}

func (f *FakeBackend) FakeSetOpenError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openErr = err
}

func (f *FakeBackend) FakeSetOpenGate(gate <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openGate = gate
}

func (f *FakeBackend) FakeOpenedSpecs() []world.SpawnSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]world.SpawnSpec(nil), f.opens...)
}

// FakeCloseStage identifies the four ordered phases of Lease.Close.
type FakeCloseStage string

const (
	FakeCloseProcessStop  FakeCloseStage = "process-stop"
	FakeCloseEffectsDrain FakeCloseStage = "effects-drain"
	FakeCloseEffectsAck   FakeCloseStage = "effects-ack"
	FakeCloseCleanup      FakeCloseStage = "mount-network-cleanup"
)

var fakeCloseStages = []FakeCloseStage{
	FakeCloseProcessStop,
	FakeCloseEffectsDrain,
	FakeCloseEffectsAck,
	FakeCloseCleanup,
}

// FakeLease exposes fixed values and models only lifecycle ordering, explicit
// effects, gates, and injected errors. It provides no isolation.
type FakeLease struct {
	endpoint world.Endpoint
	metadata world.SpawnMetadata
	upperDir string
	effects  []world.EffectAttempt

	mu           sync.Mutex
	stageErrors  map[FakeCloseStage]error
	stageGates   map[FakeCloseStage]<-chan struct{}
	stageReached map[FakeCloseStage]chan struct{}
	closeOrder   []FakeCloseStage
	closeDone    chan struct{}
	closeErr     error
}

func NewFakeLease(
	endpoint world.Endpoint,
	metadata world.SpawnMetadata,
	upperDir string,
	effects []world.EffectAttempt,
) *FakeLease {
	reached := map[FakeCloseStage]chan struct{}{}
	for _, stage := range fakeCloseStages {
		reached[stage] = make(chan struct{})
	}
	return &FakeLease{
		endpoint: endpoint, metadata: metadata.Clone(), upperDir: upperDir,
		effects:      append([]world.EffectAttempt(nil), effects...),
		stageErrors:  map[FakeCloseStage]error{},
		stageGates:   map[FakeCloseStage]<-chan struct{}{},
		stageReached: reached,
	}
}

func (f *FakeLease) AdapterEndpoint() world.Endpoint { return f.endpoint }
func (f *FakeLease) Metadata() world.SpawnMetadata   { return f.metadata.Clone() }
func (f *FakeLease) UpperDir() string                { return f.upperDir }

func (f *FakeLease) Effects() <-chan world.EffectAttempt {
	out := make(chan world.EffectAttempt, len(f.effects))
	for _, effect := range f.effects {
		out <- effect
	}
	close(out)
	return out
}

// Close records and executes every lifecycle stage in contract order. Stage
// errors are joined instead of short-circuiting cleanup. A stage gate can be
// used to make ordering deterministic without sleeping.
func (f *FakeLease) Close(ctx context.Context) error {
	f.mu.Lock()
	if f.closeDone != nil {
		done := f.closeDone
		f.mu.Unlock()
		select {
		case <-done:
			f.mu.Lock()
			err := f.closeErr
			f.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.closeDone = make(chan struct{})
	done := f.closeDone
	f.mu.Unlock()

	var joined error
	contextRecorded := false
	for _, stage := range fakeCloseStages {
		f.mu.Lock()
		f.closeOrder = append(f.closeOrder, stage)
		close(f.stageReached[stage])
		gate, stageErr := f.stageGates[stage], f.stageErrors[stage]
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				if !contextRecorded {
					joined = errors.Join(joined, ctx.Err())
					contextRecorded = true
				}
			}
		}
		joined = errors.Join(joined, stageErr)
	}

	f.mu.Lock()
	f.closeErr = joined
	close(done)
	f.mu.Unlock()
	return joined
}

func (f *FakeLease) FakeSetCloseError(stage FakeCloseStage, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageErrors[stage] = err
}

func (f *FakeLease) FakeSetCloseGate(stage FakeCloseStage, gate <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageGates[stage] = gate
}

func (f *FakeLease) FakeCloseOrder() []FakeCloseStage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCloseStage(nil), f.closeOrder...)
}

func (f *FakeLease) FakeStageReached(stage FakeCloseStage) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stageReached[stage]
}
