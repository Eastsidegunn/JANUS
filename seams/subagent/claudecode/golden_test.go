package claudecode

import (
	"bufio"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
)

var update = flag.Bool("update-golden", false, "골든 파일을 재생성한다")

const fixtureDir = "../../../contracts/fixtures/claude-code"

// goldenFile은 한 픽스처의 변환 결과 전체다. 무시된 네이티브 이벤트를 함께
// 기록해 의도적 무시와 조용한 누락을 구분한다 (제안서 §8.1).
type goldenFile struct {
	Fixture string        `json:"fixture"`
	Events  []goldenEvent `json:"events"`
	Ignored []string      `json:"unmapped_native_lines"`
}

type goldenEvent struct {
	Kind    gen.EventKind   `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	RawB64  string          `json:"raw_b64"`
}

func convertFixture(t *testing.T, path string) goldenFile {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)

	p := NewParser()
	out := goldenFile{Fixture: filepath.Base(path)}
	for sc.Scan() {
		line := sc.Bytes()
		before := len(out.Events)
		evs, err := p.ParseLine(line)
		if err != nil {
			t.Fatalf("%s: 변환 실패: %v\n줄: %.200s", path, err, line)
		}
		for _, e := range evs {
			out.Events = append(out.Events, goldenEvent{Kind: e.Kind, Payload: e.Payload, RawB64: RawB64(e.Raw)})
		}
		if len(out.Events) == before {
			d := p.Disposition()
			if d == "" {
				t.Fatalf("%s: %s 줄이 이벤트도 사유도 남기지 않음 — 조용한 누락", path, nativeLabel(line))
			}
			out.Ignored = append(out.Ignored, nativeLabel(line)+" → "+d)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func nativeLabel(line []byte) string {
	var n struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	json.Unmarshal(line, &n)
	if n.Subtype != "" {
		return n.Type + "/" + n.Subtype
	}
	return n.Type
}

// T9 완료 기준: T8 Claude 픽스처 전체의 정규화 결과 골든 대조.
func TestGoldenClaudeFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.ndjson"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("픽스처를 찾지 못함: %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 8 {
		t.Fatalf("픽스처 %d건 (8건 기대)", len(paths))
	}
	vals, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".ndjson")
		t.Run(name, func(t *testing.T) {
			got := convertFixture(t, path)

			// 방출된 모든 이벤트는 §5.2 계약을 통과해야 한다.
			for i, e := range got.Events {
				line, err := json.Marshal(map[string]any{
					"v": 1, "kind": e.Kind, "payload": e.Payload, "raw": e.RawB64,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := vals.ValidateEvent(line); err != nil {
					t.Fatalf("이벤트 %d(%s)가 §5.2 위반: %v", i, e.Kind, err)
				}
			}

			goldenPath := filepath.Join("testdata", "golden", name+".json")
			want, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			if *update {
				os.MkdirAll(filepath.Dir(goldenPath), 0o755)
				if err := os.WriteFile(goldenPath, want, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Log("골든 갱신:", goldenPath)
				return
			}
			have, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("골든 없음 (%v) — -update-golden으로 생성", err)
			}
			if string(have) != string(want) {
				t.Errorf("골든 불일치: %s\n--- got ---\n%s", goldenPath, want)
			}
		})
	}
}

// FR-ADP-06: 매핑 대상 중간 이벤트 유실 0 — 네이티브 tool_use/tool_result 수와
// 정규화 tool_call/tool_result 수가 일치한다(거부 경로 포함).
func TestNoIntermediateEventLoss(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join(fixtureDir, "*.ndjson"))
	sort.Strings(paths)
	filesWithTools := 0
	for _, path := range paths {
		nativeUse, nativeResult := countNative(t, path)
		got := convertFixture(t, path)
		var calls, results int
		for _, e := range got.Events {
			switch e.Kind {
			case gen.EventKindSubagentToolCall:
				calls++
			case gen.EventKindSubagentToolResult:
				results++
			}
		}
		if calls != nativeUse {
			t.Errorf("%s: tool_call %d ≠ 네이티브 tool_use %d", filepath.Base(path), calls, nativeUse)
		}
		if results != nativeResult {
			t.Errorf("%s: tool_result %d ≠ 네이티브 tool_result %d", filepath.Base(path), results, nativeResult)
		}
		if nativeUse > 0 {
			filesWithTools++
		}
	}
	// 제안서 §6의 근거 수치 고정 (tool 이벤트 보유 파일 6건)
	if filesWithTools != 6 {
		t.Errorf("tool 이벤트 보유 파일 %d건 (6건 기대)", filesWithTools)
	}
}

func countNative(t *testing.T, path string) (toolUse, toolResult int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		var n struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &n) != nil {
			continue
		}
		for _, c := range n.Message.Content {
			switch c.Type {
			case "tool_use":
				toolUse++
			case "tool_result":
				toolResult++
			}
		}
	}
	return toolUse, toolResult
}
