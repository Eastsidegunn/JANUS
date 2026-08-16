// boundarylint는 §3.1의 의존 방향 규칙을 강제한다:
// contracts ← core ← seams ← surfaces, seam 간 수평 import 금지,
// collector는 core와 코드 경로를 공유하지 않는다.
//
// `go list -json ./...`의 import 그래프를 검사한다. 테스트 전용
// import(TestImports, XTestImports)를 포함하고, build tag 사각지대를
// 줄이기 위해 GOOS를 linux·darwin 두 번 순회한다(결과는 중복 제거).
// 허용된 최상위 디렉토리 밖의 패키지는 존재만으로 위반이다.
// 위반이 있으면 종료 코드 1.
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

// sweepGOOS는 린트가 순회하는 대상 플랫폼이다. CI(리눅스)와
// 개발 머신(macOS) 어느 쪽에서 돌려도 같은 그래프가 검사된다.
var sweepGOOS = []string{"linux", "darwin"}

func main() {
	module, err := modulePath(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "boundarylint:", err)
		os.Exit(2)
	}
	var pkgs []Pkg
	uniq := map[string]bool{}
	for _, goos := range sweepGOOS {
		got, err := listPackages(".", goos)
		if err != nil {
			fmt.Fprintln(os.Stderr, "boundarylint:", err)
			os.Exit(2)
		}
		pkgs = append(pkgs, got...)
		for _, p := range got {
			uniq[p.ImportPath] = true
		}
	}
	violations := Check(module, pkgs)
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "boundarylint: %d개 경계 위반 (§3.1):\n", len(violations))
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v)
		}
		os.Exit(1)
	}
	fmt.Printf("boundarylint: ok (%d개 패키지, GOOS=%s)\n", len(uniq), strings.Join(sweepGOOS, ","))
}

func modulePath(dir string) (string, error) {
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// listPackages는 지정 GOOS 기준의 패키지 그래프를 반환한다.
// -e: 해당 GOOS에서 빌드 제외되는 패키지가 있어도 목록 자체는 얻는다
// (빌드 오류의 검출은 vet/test의 몫이고, 여기서는 그래프만 필요하다).
func listPackages(dir, goos string) ([]Pkg, error) {
	cmd := exec.Command("go", "list", "-e",
		"-json=ImportPath,Imports,TestImports,XTestImports", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go list ./... (GOOS=%s): %s", goos, ee.Stderr)
		}
		return nil, fmt.Errorf("go list ./... (GOOS=%s): %w", goos, err)
	}
	var pkgs []Pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p Pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list 출력 파싱 (GOOS=%s): %w", goos, err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}
