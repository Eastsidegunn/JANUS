// Package worldtest contains test-only fakes. They provide no OCI, overlay, or
// network isolation and must never be imported by production files.
package worldtest

import (
	"context"
	"errors"
	"sync"

	"github.com/Eastsidegunn/JANUS/core/world"
)

var (
	_ world.Backend       = (*FakeBackend)(nil)
	_ world.PreparedLease = (*FakePreparedLease)(nil)
	_ world.ActiveLease   = (*FakeActiveLease)(nil)
)

// FakeBackend records Prepare calls and returns a configured prepared fake.
type FakeBackend struct {
	mu         sync.Mutex
	prepared   *FakePreparedLease
	prepareErr error
	gate       <-chan struct{}
	prepares   []world.SpawnSpec
}

func NewFakeBackend(prepared *FakePreparedLease) *FakeBackend {
	if prepared == nil {
		prepared = NewFakePreparedLease(world.SpawnMetadata{}, "", nil)
	}
	return &FakeBackend{prepared: prepared}
}

func (f *FakeBackend) Prepare(ctx context.Context, spec world.SpawnSpec) (world.PreparedLease, error) {
	f.mu.Lock()
	f.prepares = append(f.prepares, spec)
	gate, prepareErr, prepared := f.gate, f.prepareErr, f.prepared
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if prepareErr != nil {
		return nil, prepareErr
	}
	return prepared, nil
}

func (f *FakeBackend) FakeSetPrepareError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareErr = err
}

func (f *FakeBackend) FakeSetPrepareGate(gate <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = gate
}

func (f *FakeBackend) FakePreparedSpecs() []world.SpawnSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]world.SpawnSpec(nil), f.prepares...)
}

// FakePreparedLease models the receipt gate without starting a process.
type FakePreparedLease struct {
	id       world.PreparedID
	metadata world.SpawnMetadata
	upperDir string
	active   *FakeActiveLease

	mu          sync.Mutex
	activateErr error
	abortErr    error
	activated   bool
	aborted     bool
	activations int
}

func NewFakePreparedLease(metadata world.SpawnMetadata, upperDir string, active *FakeActiveLease) *FakePreparedLease {
	id, err := world.NewPreparedID("2222222222222222")
	if err != nil {
		panic(err)
	}
	if active == nil {
		active = NewFakeActiveLease(world.ProcessEndpoint{}, world.ApprovalEndpoint{}, upperDir, nil)
	}
	return &FakePreparedLease{id: id, metadata: metadata.Clone(), upperDir: upperDir, active: active}
}

func (f *FakePreparedLease) ID() world.PreparedID          { return f.id }
func (f *FakePreparedLease) Metadata() world.SpawnMetadata { return f.metadata.Clone() }
func (f *FakePreparedLease) UpperDir() string              { return f.upperDir }
func (f *FakePreparedLease) FakeActivationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activations
}
func (f *FakePreparedLease) FakeAborted() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.aborted }
func (f *FakePreparedLease) FakeSetActivateError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activateErr = err
}
func (f *FakePreparedLease) FakeSetAbortError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortErr = err
}

func (f *FakePreparedLease) Activate(_ context.Context, receipt world.SpawnReceipt) (world.ActiveLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted || f.activated {
		return nil, errors.New("worldtest: prepared lease가 이미 소비됨")
	}
	if err := world.ValidateSpawnReceipt(receipt, f.id, f.metadata); err != nil {
		return nil, err
	}
	if f.activateErr != nil {
		return nil, f.activateErr
	}
	f.activated = true
	f.activations++
	return f.active, nil
}

func (f *FakePreparedLease) Abort(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activated {
		return errors.New("worldtest: active lease는 Abort할 수 없음")
	}
	f.aborted = true
	return f.abortErr
}

// FakeCloseStage identifies the four ordered phases of ActiveLease.Close.
type FakeCloseStage string

const (
	FakeCloseProcessStop  FakeCloseStage = "process-stop"
	FakeCloseEffectsDrain FakeCloseStage = "effects-drain"
	FakeCloseEffectsAck   FakeCloseStage = "effects-ack"
	FakeCloseCleanup      FakeCloseStage = "mount-network-cleanup"
)

var fakeCloseStages = []FakeCloseStage{
	FakeCloseProcessStop, FakeCloseEffectsDrain, FakeCloseEffectsAck, FakeCloseCleanup,
}

// FakeActiveLease exposes fixed descriptors and lifecycle controls only.
type FakeActiveLease struct {
	process  world.ProcessEndpoint
	approval world.ApprovalEndpoint
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

func NewFakeActiveLease(process world.ProcessEndpoint, approval world.ApprovalEndpoint, upperDir string, effects []world.EffectAttempt) *FakeActiveLease {
	reached := map[FakeCloseStage]chan struct{}{}
	for _, stage := range fakeCloseStages {
		reached[stage] = make(chan struct{})
	}
	return &FakeActiveLease{
		process: process, approval: approval, upperDir: upperDir,
		effects:     append([]world.EffectAttempt(nil), effects...),
		stageErrors: map[FakeCloseStage]error{}, stageGates: map[FakeCloseStage]<-chan struct{}{},
		stageReached: reached,
	}
}

func (f *FakeActiveLease) ProcessEndpoint() world.ProcessEndpoint   { return f.process }
func (f *FakeActiveLease) ApprovalEndpoint() world.ApprovalEndpoint { return f.approval }
func (f *FakeActiveLease) UpperDir() string                         { return f.upperDir }

func (f *FakeActiveLease) Effects() <-chan world.EffectAttempt {
	out := make(chan world.EffectAttempt, len(f.effects))
	for _, effect := range f.effects {
		out <- effect
	}
	close(out)
	return out
}

func (f *FakeActiveLease) Close(ctx context.Context) error {
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

func (f *FakeActiveLease) FakeSetCloseError(stage FakeCloseStage, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageErrors[stage] = err
}
func (f *FakeActiveLease) FakeSetCloseGate(stage FakeCloseStage, gate <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageGates[stage] = gate
}
func (f *FakeActiveLease) FakeCloseOrder() []FakeCloseStage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCloseStage(nil), f.closeOrder...)
}
func (f *FakeActiveLease) FakeStageReached(stage FakeCloseStage) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stageReached[stage]
}
