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
		"01-simple-text.ndjson":  "bfc02444630a9d74ffb0234628ca753d95bb1a9006cfbaea4d79919ed00a28e3",
		"02-single-tool.ndjson":  "2ca85dd392c34be9ea51b61dd8e736e9a71006533e4235043f85fcfbd105b31a",
		"03-multi-tool.ndjson":   "36306702db6482253bf998e9d90e74d172b372d52354072d7dafe8c9629b3ca4",
		"04-edit-file.ndjson":    "1a22a1e6fb50b15e509cef6010a6ef594bcabbcc9992ede0a0570f141a0baecb",
		"06-tool-error.ndjson":   "4b25666663c789d51540f0d89cf672c3c28c5133362c84a22f163358fe69f62d",
		"07-command-fail.ndjson": "a1d30fdaaa1eb172d56490e7d31cc0f0aeb11277c369f5c455076ef97f2609b3",
		"08-interrupted.ndjson":  "117e9f91d2960f9cdffd937e82d833ef873b2a4f3c870b344a6b9b850c1c5480",
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
