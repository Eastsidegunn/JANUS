// boundarylint는 §3.1의 의존 방향 규칙을 강제한다:
// contracts ← core ← seams ← surfaces, seam 간 수평 import 금지,
// collector는 core와 코드 경로를 공유하지 않는다.
//
// `go list -json ./...`의 실제 import 그래프를 검사하므로
// 빌드에 포함되는 모든 import가 대상이다. 위반이 있으면 종료 코드 1.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	module, pkgs, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "boundarylint:", err)
		os.Exit(2)
	}
	violations := Check(module, pkgs)
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "boundarylint: %d개 의존 방향 위반 (§3.1):\n", len(violations))
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v)
		}
		os.Exit(1)
	}
	fmt.Printf("boundarylint: ok (%d개 패키지)\n", len(pkgs))
}

func load() (string, []Pkg, error) {
	modOut, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return "", nil, fmt.Errorf("go list -m: %w", err)
	}
	module := strings.TrimSpace(string(modOut))

	listOut, err := exec.Command("go", "list", "-json=ImportPath,Imports", "./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", nil, fmt.Errorf("go list ./...: %s", ee.Stderr)
		}
		return "", nil, fmt.Errorf("go list ./...: %w", err)
	}

	var pkgs []Pkg
	dec := json.NewDecoder(strings.NewReader(string(listOut)))
	for {
		var p Pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return "", nil, fmt.Errorf("go list 출력 파싱: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return module, pkgs, nil
}
