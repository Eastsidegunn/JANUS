package main

import (
	"fmt"
	"sort"
	"strings"
)

// Pkg는 `go list -json` 출력 중 린트에 필요한 최소 단면이다.
// 테스트 전용 import(TestImports, XTestImports)도 검사 대상이다.
type Pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// validLayers는 §3.1이 허용하는 최상위 디렉토리 전부다.
// 이 집합 밖의 패키지는 import 여부와 무관하게 존재만으로 위반이다 —
// 미분류 패키지가 경계 검사를 우회하는 것을 막는다.
var validLayers = map[string]bool{
	"contracts": true,
	"core":      true,
	"seams":     true,
	"collector": true,
	"surfaces":  true,
	"tools":     true,
}

// Check는 패키지 그래프가 §3.1의 의존 방향 규칙을 지키는지 검사한다.
// 위반 목록을 중복 제거·정렬해 반환한다(같은 패키지가 여러 GOOS 순회로
// 중복 전달돼도 동일 위반은 한 번만 나온다). 비어 있으면 통과다.
func Check(module string, pkgs []Pkg) []string {
	seen := map[string]bool{}
	var violations []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			violations = append(violations, v)
		}
	}
	for _, p := range pkgs {
		from, ok := rel(module, p.ImportPath)
		if !ok {
			continue
		}
		fromLayer, _ := classify(from)
		if !validLayers[fromLayer] {
			add(fmt.Sprintf("미분류 최상위 디렉토리: %s (허용: contracts|core|seams|collector|surfaces|tools)", from))
			continue
		}
		for _, imp := range allImports(p) {
			to, ok := rel(module, imp)
			if !ok {
				continue
			}
			if !allowed(from, to) {
				add(fmt.Sprintf("%s → %s", from, to))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

func allImports(p Pkg) []string {
	out := make([]string, 0, len(p.Imports)+len(p.TestImports)+len(p.XTestImports))
	out = append(out, p.Imports...)
	out = append(out, p.TestImports...)
	out = append(out, p.XTestImports...)
	return out
}

// rel은 모듈 내부 패키지 경로를 모듈 기준 상대 경로로 바꾼다.
// 외부 패키지(표준 라이브러리 포함)면 ok=false.
func rel(module, path string) (string, bool) {
	if path == module {
		return ".", true
	}
	if rest, found := strings.CutPrefix(path, module+"/"); found {
		return rest, true
	}
	return "", false
}

// classify는 상대 경로에서 (층, seam 이름)을 얻는다.
// seam 이름은 seams/<이름>/... 형태일 때만 비어 있지 않다.
func classify(relPath string) (layer, seam string) {
	parts := strings.Split(relPath, "/")
	layer = parts[0]
	if layer == "seams" && len(parts) > 1 {
		seam = parts[1]
	}
	return layer, seam
}

// allowed는 from 패키지가 to 패키지를 import해도 되는지 판정한다.
//
// 규칙 (§3.1, 시스템 불변식 "의존은 아래로만"):
//   - contracts: 내부 의존 없음 (contracts 내부 상호 참조만 허용)
//   - core: contracts만
//   - seams/<x>: contracts, core, 그리고 같은 seam(<x>) 내부만.
//     다른 seam·seams 루트 패키지로의 수평 import 금지.
//   - collector: contracts만 — core와 코드 경로를 공유하지 않는다.
//   - surfaces: 조립 지점. 명시된 층(contracts, core, seams, collector,
//     surfaces)만 허용 — 미분류 대상은 허용되지 않는다.
//   - tools: 개발 도구. 내부 층 import 금지 (tools 내부만).
//   - 그 외 알 수 없는 최상위 디렉토리는 어느 방향으로도 허용되지 않는다.
func allowed(from, to string) bool {
	fromLayer, fromSeam := classify(from)
	toLayer, toSeam := classify(to)
	switch fromLayer {
	case "contracts":
		return toLayer == "contracts"
	case "core":
		return toLayer == "core" || toLayer == "contracts"
	case "seams":
		if toLayer == "contracts" || toLayer == "core" {
			return true
		}
		return toLayer == "seams" && fromSeam == toSeam && fromSeam != ""
	case "collector":
		return toLayer == "collector" || toLayer == "contracts"
	case "surfaces":
		return toLayer == "contracts" || toLayer == "core" || toLayer == "seams" ||
			toLayer == "collector" || toLayer == "surfaces"
	case "tools":
		return toLayer == "tools"
	default:
		return false
	}
}
