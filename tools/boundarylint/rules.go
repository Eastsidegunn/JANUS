package main

import (
	"fmt"
	"sort"
	"strings"
)

// Pkg는 `go list -json` 출력 중 린트에 필요한 최소 단면이다.
type Pkg struct {
	ImportPath string
	Imports    []string
}

// Check는 모듈 내부 import가 §3.1의 의존 방향 규칙을 지키는지 검사한다.
// 위반 목록을 정렬해 반환한다. 비어 있으면 통과다.
func Check(module string, pkgs []Pkg) []string {
	var violations []string
	for _, p := range pkgs {
		from, ok := rel(module, p.ImportPath)
		if !ok {
			continue
		}
		for _, imp := range p.Imports {
			to, ok := rel(module, imp)
			if !ok {
				continue
			}
			if !allowed(from, to) {
				violations = append(violations,
					fmt.Sprintf("%s → %s", from, to))
			}
		}
	}
	sort.Strings(violations)
	return violations
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
//   - surfaces: 조립 지점. tools를 제외한 모든 층 허용.
//   - tools: 개발 도구. 내부 층 import 금지 (tools 내부만).
//   - 그 외 알 수 없는 최상위 디렉토리의 내부 import는 전부 위반.
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
		return toLayer != "tools"
	case "tools":
		return toLayer == "tools"
	default:
		return false
	}
}
