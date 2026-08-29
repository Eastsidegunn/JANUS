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
	// StandardDeps is populated from go list dependency metadata. It is kept
	// on each package record so Check can distinguish stdlib from arbitrary
	// external modules without guessing from a dotted path.
	StandardDeps map[string]bool
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

// externalRestrictions는 특정 외부 모듈의 import를 지정 내부 패키지로
// 한정한다 — 문서 선언이 아니라 린트로 강제한다 (T3 [H] 승인 조건).
// 키는 import 경로 접두사, 값은 허용되는 내부 패키지(및 그 하위) 목록.
var externalRestrictions = map[string][]string{
	"modernc.org/":                          {"seams/store/sqlite"}, // SQLite 드라이버 (T3 제안서)
	"github.com/santhosh-tekuri/jsonschema": {"contracts/validate"}, // 스키마 검증기 (T1 제안서 §6)
	"github.com/goccy/go-yaml":              {"core/policy"},        // YAML 파서 (T6 제안서)
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
		checkImport := func(imp string, testOnly bool) {
			to, ok := rel(module, imp)
			if !ok {
				if v := checkExternal(from, imp); v != "" {
					add(v)
				} else if fromLayer == "collector" && !isStandardImport(imp, p.StandardDeps) {
					add(fmt.Sprintf("%s → %s (collector는 표준 라이브러리와 contracts만 import 가능)", from, imp))
				}
				return
			}
			if !testOnly && isWorldtest(to) && !isWorldtest(from) {
				add(fmt.Sprintf("%s → %s (worldtest는 _test.go에서만 import 가능)", from, to))
				return
			}
			if isAudit(from) && isAuditForbidden(to) {
				add(fmt.Sprintf("%s → %s (audit는 contracts와 자체 value type만 import 가능)", from, to))
				return
			}
			if !allowed(from, to) {
				add(fmt.Sprintf("%s → %s", from, to))
			}
		}
		for _, imp := range p.Imports {
			checkImport(imp, false)
		}
		for _, imp := range p.TestImports {
			checkImport(imp, true)
		}
		for _, imp := range p.XTestImports {
			checkImport(imp, true)
		}
	}
	sort.Strings(violations)
	return violations
}

func isAudit(path string) bool {
	return path == "core/audit" || strings.HasPrefix(path, "core/audit/")
}

func isAuditForbidden(path string) bool {
	return path == "collector" || strings.HasPrefix(path, "collector/") ||
		path == "core/logd" || strings.HasPrefix(path, "core/logd/") ||
		path == "seams" || strings.HasPrefix(path, "seams/") ||
		path == "surfaces" || strings.HasPrefix(path, "surfaces/")
}

func isStandardImport(path string, metadata map[string]bool) bool {
	if metadata != nil {
		standard, known := metadata[path]
		return known && standard
	}
	// Unit tests may construct Pkg values directly without go list metadata.
	// In production listPackages always supplies the complete map; this
	// conservative fallback preserves the conventional stdlib path shape for
	// those isolated rule tests.
	first := path
	if i := strings.IndexByte(first, '/'); i >= 0 {
		first = first[:i]
	}
	return !strings.Contains(first, ".")
}

func isWorldtest(path string) bool {
	return path == "core/world/worldtest" || strings.HasPrefix(path, "core/world/worldtest/")
}

// checkExternal은 제한된 외부 모듈 import가 허용 패키지 밖에서 일어나면
// 위반 문자열을, 아니면 빈 문자열을 반환한다.
func checkExternal(from, imp string) string {
	for prefix, allowedPkgs := range externalRestrictions {
		if !strings.HasPrefix(imp, prefix) {
			continue
		}
		for _, a := range allowedPkgs {
			if from == a || strings.HasPrefix(from, a+"/") {
				return ""
			}
		}
		return fmt.Sprintf("%s → %s (외부 모듈 %s*는 %s에서만 import 가능)",
			from, imp, prefix, strings.Join(allowedPkgs, ", "))
	}
	return ""
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
