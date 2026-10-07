// fixtureprint는 T8 픽스처의 원본 포맷 fingerprint를 대조한다 (FR-ADP-05).
//
// 대상 도구의 출력 포맷이 바뀌면 CI에서 검출되도록, 파일별 줄 수·SHA-256·
// top-level type 히스토그램을 골든과 비교한다. Claude 8건의 정규화 골든은
// seams/subagent/claudecode의 테스트가 담당하고, 이 도구는 Codex 7건을 포함한
// 15건 전체의 원본성을 본다 (제안서 §8.1의 2층).
//
// 사용법:
//
//	fixtureprint            # 골든과 대조 (불일치 시 exit 1)
//	fixtureprint -update    # 골든 재생성
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	fixtureRoot = "contracts/fixtures"
	goldenPath  = "contracts/fixtures/fingerprint.json"
	maxLine     = 4 << 20
)

type fingerprint struct {
	Lines  int            `json:"lines"`
	SHA256 string         `json:"sha256"`
	Types  map[string]int `json:"top_level_types"`
}

func main() {
	update := flag.Bool("update", false, "골든 재생성")
	flag.Parse()
	got, err := scan()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixtureprint:", err)
		os.Exit(2)
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixtureprint:", err)
		os.Exit(2)
	}
	encoded = append(encoded, '\n')
	if *update {
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fixtureprint:", err)
			os.Exit(2)
		}
		fmt.Printf("fixtureprint: 골든 갱신 (%d개 픽스처)\n", len(got))
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixtureprint: 골든 없음(%v) — -update로 생성\n", err)
		os.Exit(1)
	}
	if string(want) != string(encoded) {
		fmt.Fprintln(os.Stderr, "fixtureprint: 픽스처 fingerprint 불일치 — 대상 도구 출력 포맷 변경 또는 픽스처 훼손")
		fmt.Fprintln(os.Stderr, "--- 현재 ---")
		os.Stderr.Write(encoded)
		os.Exit(1)
	}
	fmt.Printf("fixtureprint: %d개 픽스처 fingerprint 일치\n", len(got))
}

func scan() (map[string]fingerprint, error) {
	out := map[string]fingerprint{}
	var paths []string
	err := filepath.Walk(fixtureRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(p) == ".ndjson" {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("픽스처가 없음: %s", fixtureRoot)
	}
	for _, p := range paths {
		fp, err := fingerprintFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		rel, err := filepath.Rel(fixtureRoot, p)
		if err != nil {
			return nil, err
		}
		out[rel] = fp
	}
	return out, nil
}

func fingerprintFile(path string) (fingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()

	h := sha256.New()
	tee := io.TeeReader(f, h)
	sc := bufio.NewScanner(tee)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	types := map[string]int{}
	lines := 0
	for sc.Scan() {
		lines++
		var n struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
			return fingerprint{}, fmt.Errorf("%d행 JSON 파싱: %w", lines, err)
		}
		key := n.Type
		if n.Subtype != "" {
			key = n.Type + "/" + n.Subtype
		}
		types[key]++
	}
	if err := sc.Err(); err != nil {
		return fingerprint{}, err
	}
	return fingerprint{Lines: lines, SHA256: hex.EncodeToString(h.Sum(nil)), Types: types}, nil
}
