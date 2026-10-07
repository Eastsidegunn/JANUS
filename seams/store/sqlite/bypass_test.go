package sqlite_test

// 외부 패키지 시점의 우회 회귀 테스트 — 재리뷰가 실증한 type assertion
// 우회(l.Reader.(logd.Store) → 임의 seq·미마스킹 Append)의 재발 방지.
// 반드시 package sqlite_test여야 한다: 내부 패키지에서는 비공개 타입에
// 접근할 수 있어 외부 노출 여부를 증명하지 못한다.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	sqlite "github.com/Eastsidegunn/JANUS/seams/store/sqlite"
)

func TestReaderExposesNoMutationCapability(t *testing.T) {
	ctx := context.Background()
	l, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if _, ok := l.Reader.(logd.Store); ok {
		t.Fatal("Reader가 mutation capability(logd.Store)를 노출함 — type assertion 우회 가능")
	}
	if _, ok := l.Reader.(interface {
		Append(context.Context, gen.EventRecord) error
	}); ok {
		t.Fatal("Reader의 동적 타입이 Append를 구현함")
	}
	if _, ok := l.Reader.(interface{ Close() error }); ok {
		t.Fatal("Reader의 동적 타입이 Close를 노출함")
	}
}
