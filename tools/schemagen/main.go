// schemagen: contracts JSON Schema → Go 타입 생성기.
//
// 사용법: schemagen -out <디렉토리> <schema.json>[:<루트타입이름>] ...
//
// 입력 파일마다 <이름>.gen.go 하나를 출력한다(events.schema.json → events.gen.go).
// 루트타입이름이 주어지면 문서 루트 객체도 해당 이름의 struct로 생성한다.
// 승인 서브셋(docs/t1-codegen-proposal.md §2) 밖의 키워드·구조는 생성 실패다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	outDir := flag.String("out", "", "생성 파일 출력 디렉토리")
	flag.Parse()
	if *outDir == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "사용법: schemagen -out <dir> <schema.json>[:<RootType>] ...")
		os.Exit(2)
	}
	if err := run(*outDir, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(outDir string, args []string) error {
	seen := map[string]string{} // 방출 이름 → 출처 파일 (파일 간 충돌 검사)
	for _, arg := range args {
		path, rootType, _ := strings.Cut(arg, ":")
		root, err := loadSchema(path)
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		src, emitted, err := Generate(root, base, rootType)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, name := range emitted {
			if prev, dup := seen[name]; dup {
				return fmt.Errorf("%s: 타입 이름 충돌 %q (이미 %s에서 방출)", base, name, prev)
			}
			seen[name] = base
		}
		outName := strings.TrimSuffix(base, ".schema.json") + ".gen.go"
		if err := os.WriteFile(filepath.Join(outDir, outName), src, 0o644); err != nil {
			return err
		}
		fmt.Printf("schemagen: %s → %s (%d개 타입·상수)\n", base, outName, len(emitted))
	}
	return nil
}

// loadSchema는 UseNumber로 디코딩한다 — const·maximum의 정수를
// float64 반올림 없이 보존해야 한다(int64 상한 9223372036854775807).
func loadSchema(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%s: JSON 파싱: %w", path, err)
	}
	return root, nil
}
