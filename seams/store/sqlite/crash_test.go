package sqlite

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// NFR-02 크래시 복구: helper 프로세스가 커밋 acknowledge 직후 Close 없이
// SIGKILL로 죽어도, 부모가 DB를 다시 열면 마지막 ack된 seq까지 복원된다.
// (정상 close/reopen은 checkpoint가 개입할 수 있어 크래시 실증이 아니다 —
// T3 제안서 §9의 [H] 리뷰 반영.)
func TestCrashRecovery(t *testing.T) {
	if os.Getenv("HX_CRASH_HELPER") == "1" {
		crashHelper()
		return // unreachable — helper는 SIGKILL로 끝난다
	}

	path := filepath.Join(t.TempDir(), "crash.db")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestCrashRecovery$")
	cmd.Env = append(os.Environ(), "HX_CRASH_HELPER=1", "HX_CRASH_DB="+path)
	out, runErr := cmd.Output()
	if runErr == nil {
		t.Fatal("helper가 정상 종료함 — SIGKILL 크래시가 일어나지 않았다")
	}
	var lastAcked int64
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "ACKED "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			lastAcked = n
		}
	}
	if lastAcked < 1 {
		t.Fatalf("helper가 ack를 보고하지 않음. 출력:\n%s", out)
	}

	l, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("크래시 후 reopen 실패: %v", err)
	}
	defer l.Close()
	last, err := l.Reader.LastSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if last < lastAcked {
		t.Fatalf("복원 seq %d < 마지막 ack %d — acknowledge된 커밋이 유실됨 (NFR-02 위반)", last, lastAcked)
	}
	events, err := l.Reader.ReadFrom(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(events)) != last {
		t.Fatalf("이벤트 %d건, LastSeq %d — 로그 구멍", len(events), last)
	}
	t.Logf("크래시 복구 확인: ack=%d, 복원=%d", lastAcked, last)
}

// crashHelper는 자식 프로세스 모드다: store+writer로 이벤트를 커밋하고
// ack된 seq를 stdout에 기록한 뒤 스스로 SIGKILL한다 — defer/Close 없음.
func crashHelper() {
	path := os.Getenv("HX_CRASH_DB")
	l, err := Open(context.Background(), path)
	if err != nil {
		fmt.Println("HELPER_ERR", err)
		os.Exit(3)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 0; i < 5; i++ {
		seq, err := l.Writer.Submit(context.Background(), rec(0, fmt.Sprintf(`{"i":%d}`, i)))
		if err != nil {
			fmt.Fprintln(out, "HELPER_ERR", err)
			out.Flush()
			os.Exit(3)
		}
		fmt.Fprintf(out, "ACKED %d\n", seq)
	}
	out.Flush()
	os.Stdout.Sync()
	// Close 없이 즉사 — WAL checkpoint 기회를 주지 않는다
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // unreachable
}
