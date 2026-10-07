package codex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/contracts/validate"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

var updateCodexGolden = flag.Bool("update-codex-golden", false, "Codex 골든 파일을 재생성한다")

const codexFixtureDir = "../../../contracts/fixtures/codex"

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

func convertCodexFixture(t *testing.T, path string) goldenFile {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	p := NewParser(policy.ApprovalAuto)
	out := goldenFile{Fixture: filepath.Base(path)}
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		before := len(out.Events)
		events, err := p.ParseLine(line)
		if err != nil {
			t.Fatalf("%s: 변환 실패: %v", path, err)
		}
		for _, event := range events {
			out.Events = append(out.Events, goldenEvent{Kind: event.Kind, Payload: event.Payload, RawB64: RawB64(event.Raw)})
		}
		if len(out.Events) == before {
			if p.Disposition() == "" {
				t.Fatalf("%s: 이벤트도 처분 사유도 없는 조용한 누락", path)
			}
			out.Ignored = append(out.Ignored, nativeLabel(line)+" → "+p.Disposition())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, event := range mustFinish(t, p) {
		out.Events = append(out.Events, goldenEvent{Kind: event.Kind, Payload: event.Payload, RawB64: RawB64(event.Raw)})
	}
	return out
}

func mustFinish(t *testing.T, p *Parser) []Event {
	t.Helper()
	events, err := p.Finish()
	if err != nil {
		t.Fatalf("Finish 실패: %v", err)
	}
	return events
}

func nativeLabel(line []byte) string {
	var n struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &n)
	return n.Type
}

func TestGoldenCodexFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(codexFixtureDir, "*.ndjson"))
	if err != nil || len(paths) != 7 {
		t.Fatalf("Codex 픽스처 %d건 (7건 기대): %v", len(paths), err)
	}
	sort.Strings(paths)
	validator, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".ndjson")
		t.Run(name, func(t *testing.T) {
			got := convertCodexFixture(t, path)
			for i, event := range got.Events {
				wire, err := json.Marshal(map[string]any{"v": 1, "kind": event.Kind, "payload": event.Payload, "raw": event.RawB64})
				if err != nil {
					t.Fatal(err)
				}
				if err := validator.ValidateEvent(wire); err != nil {
					t.Fatalf("event %d(%s)가 §5.2 위반: %v", i, event.Kind, err)
				}
			}
			want, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			goldenPath := filepath.Join("testdata", "golden", "codex-"+name+".json")
			if *updateCodexGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, want, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			have, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("골든 없음(%v): -update-codex-golden으로 생성", err)
			}
			if string(have) != string(want) {
				t.Errorf("골든 불일치: %s\n--- got ---\n%s", goldenPath, want)
			}
		})
	}
}

func TestCodexFingerprint(t *testing.T) {
	want := map[string]string{
		"01-simple-text.ndjson":  "7e5835253e14e774b32fb3dd93b94634dfe831b86a8d93d498246d21fd94eb86",
		"02-single-tool.ndjson":  "5c517accf35221c12794e8bc787c73b431d7afc3e22b2cff4c3d474ef8e7ad3f",
		"03-multi-tool.ndjson":   "7ff8d63098df5e9cc485e3ee9363a913ff9706cfe13861448c6118700989e31c",
		"04-edit-file.ndjson":    "dad7c037b7c316bbd8382892b240ea4ee9f656c53d0c9fe4a751b3346d29ab26",
		"06-tool-error.ndjson":   "f85a8fdd021695a1f6cc7fec826a7a340aa4957ee1cc5919c218b6e22771ee20",
		"07-command-fail.ndjson": "fc55e6a481dd760b845760ce43db72fdd9a920a3dc717defeae9a8fa9359cf6b",
		"08-interrupted.ndjson":  "b8766344a6aff8c696d198771f92540555b880fb2dfe0935ef84bdd73717aca8",
	}
	for name, expected := range want {
		data, err := os.ReadFile(filepath.Join(codexFixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != expected {
			t.Errorf("%s fingerprint=%s, want %s", name, got, expected)
		}
	}
}

func TestCodexNoIntermediateEventLoss(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join(codexFixtureDir, "*.ndjson"))
	sort.Strings(paths)
	for _, path := range paths {
		var starts, completes int
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
		for sc.Scan() {
			var n struct {
				Type string `json:"type"`
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal(sc.Bytes(), &n) == nil && (n.Item.Type == "command_execution" || n.Item.Type == "file_change") {
				if n.Type == "item.started" {
					starts++
				}
				if n.Type == "item.completed" {
					completes++
				}
			}
		}
		f.Close()
		got := convertCodexFixture(t, path)
		calls, results := 0, 0
		for _, event := range got.Events {
			switch event.Kind {
			case gen.EventKindSubagentToolCall:
				calls++
			case gen.EventKindSubagentToolResult:
				results++
			}
		}
		if calls != starts || results != completes {
			t.Errorf("%s: tool call/result=%d/%d, native starts/completes=%d/%d", filepath.Base(path), calls, results, starts, completes)
		}
	}
}

func TestCodexConversionIsByteDeterministic(t *testing.T) {
	path := filepath.Join(codexFixtureDir, "04-edit-file.ndjson")
	first, err := json.Marshal(convertCodexFixture(t, path))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(convertCodexFixture(t, path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("반복 %d에서 같은 입력의 바이트 출력이 달라짐", i)
		}
	}
}
