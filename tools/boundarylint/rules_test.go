package main

import "testing"

const mod = "github.com/Eastsidegunn/JANUS"

func TestAllowed(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		// 아래 방향 의존은 허용
		{"core/logd", "contracts", true},
		{"core/logd", "contracts/gen", true},
		{"core/loop", "core/logd", true},
		{"seams/store/sqlite", "contracts", true},
		{"seams/store/sqlite", "core", true},
		{"seams/store/sqlite", "seams/store", true},
		{"collector/fsdiff", "contracts", true},
		{"collector/fsdiff", "collector", true},
		{"surfaces/cli", "contracts", true},
		{"surfaces/cli", "core", true},
		{"surfaces/cli", "seams/store/sqlite", true},
		{"surfaces/cli", "collector", true},
		{"contracts/gen", "contracts", true},
		{"tools/boundarylint", "tools/boundarylint", true},

		// 위 방향·수평 의존은 위반
		{"contracts", "core", false},
		{"contracts/gen", "seams/store", false},
		{"core/logd", "seams/store", false},
		{"core/loop", "surfaces/cli", false},
		{"core/loop", "collector", false},
		{"seams/store/sqlite", "seams/model/anthropic", false}, // seam 수평 금지
		{"seams/store/sqlite", "seams", false},                 // seams 루트로도 금지
		{"seams/store", "surfaces/cli", false},
		{"seams/store", "collector", false},
		{"collector/fsdiff", "core", false}, // collector는 core와 경로 비공유
		{"collector/fsdiff", "seams/store", false},
		{"collector/fsdiff", "surfaces/cli", false},
		{"surfaces/cli", "tools/boundarylint", false},
		{"tools/boundarylint", "core", false},

		// 미분류 대상은 어느 방향으로도 불허 (T0.1 리뷰 발견 2)
		{"unknown", "core", false},
		{"surfaces/cli", "rogue", false},
		{"core", "rogue", false},
	}
	for _, c := range cases {
		if got := allowed(c.from, c.to); got != c.want {
			t.Errorf("allowed(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: mod + "/core/logd", Imports: []string{
			"fmt",                 // 외부(표준 라이브러리)는 무시
			mod + "/contracts",    // 허용
			mod + "/seams/store",  // 위반
			mod + "/surfaces/cli", // 위반
		}},
		{ImportPath: mod + "/collector", Imports: []string{
			mod + "/core", // 위반
		}},
		{ImportPath: mod + "/surfaces/cli", Imports: []string{
			mod + "/core", mod + "/collector", // 전부 허용
		}},
	}
	assertViolations(t, Check(mod, pkgs), []string{
		"collector → core",
		"core/logd → seams/store",
		"core/logd → surfaces/cli",
	})
}

// 테스트 전용 import도 검사된다 (T0.1 리뷰 발견 1).
func TestCheckTestImports(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: mod + "/core",
			Imports:      []string{mod + "/contracts"},
			TestImports:  []string{"testing", mod + "/surfaces"},
			XTestImports: []string{mod + "/collector"},
		},
	}
	assertViolations(t, Check(mod, pkgs), []string{
		"core → collector",
		"core → surfaces",
	})
}

// 미분류 최상위 패키지는 import가 없어도 존재만으로 위반이다 (T0.1 리뷰 발견 2).
func TestCheckRoguePackage(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: mod + "/rogue", Imports: []string{"fmt"}},
		{ImportPath: mod + "/surfaces/cli", Imports: []string{mod + "/rogue"}},
	}
	assertViolations(t, Check(mod, pkgs), []string{
		"surfaces/cli → rogue",
		"미분류 최상위 디렉토리: rogue (허용: contracts|core|seams|collector|surfaces|tools)",
	})
}

// GOOS 순회로 같은 패키지가 중복 전달돼도 동일 위반은 한 번만 나온다.
func TestCheckDedup(t *testing.T) {
	p := Pkg{ImportPath: mod + "/core",
		Imports:     []string{mod + "/surfaces"},
		TestImports: []string{mod + "/surfaces"}, // 같은 edge가 두 목록에
	}
	assertViolations(t, Check(mod, []Pkg{p, p}), []string{ // 같은 패키지가 두 번
		"core → surfaces",
	})
}

// 제한된 외부 모듈은 지정 패키지(와 하위)에서만 import 가능하다 (T3 승인 조건).
func TestCheckExternalRestrictions(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: mod + "/seams/store/sqlite", Imports: []string{"modernc.org/sqlite"}},                       // 허용
		{ImportPath: mod + "/seams/store/sqlite/wal", Imports: []string{"modernc.org/sqlite"}},                   // 하위도 허용
		{ImportPath: mod + "/contracts/validate", Imports: []string{"github.com/santhosh-tekuri/jsonschema/v6"}}, // 허용
		{ImportPath: mod + "/core/policy", Imports: []string{"github.com/goccy/go-yaml"}},                        // 허용
		{ImportPath: mod + "/core/logd", Imports: []string{"modernc.org/sqlite"}},                                // 위반
		{ImportPath: mod + "/collector", TestImports: []string{"modernc.org/libc"}},                              // 테스트 import도 위반
		{ImportPath: mod + "/core", Imports: []string{"github.com/santhosh-tekuri/jsonschema/v6"}},               // 위반
		{ImportPath: mod + "/surfaces/cli", Imports: []string{"fmt", "github.com/goccy/go-yaml"}},                // fmt 무관, yaml 위반
	}
	assertViolations(t, Check(mod, pkgs), []string{
		"collector → modernc.org/libc (외부 모듈 modernc.org/*는 seams/store/sqlite에서만 import 가능)",
		"core → github.com/santhosh-tekuri/jsonschema/v6 (외부 모듈 github.com/santhosh-tekuri/jsonschema*는 contracts/validate에서만 import 가능)",
		"core/logd → modernc.org/sqlite (외부 모듈 modernc.org/*는 seams/store/sqlite에서만 import 가능)",
		"surfaces/cli → github.com/goccy/go-yaml (외부 모듈 github.com/goccy/go-yaml*는 core/policy에서만 import 가능)",
	})
}

func TestCheckClean(t *testing.T) {
	pkgs := []Pkg{
		{ImportPath: mod + "/core", Imports: []string{mod + "/contracts", "os"}},
		{ImportPath: mod + "/seams/store/sqlite", Imports: []string{mod + "/core"}},
	}
	assertViolations(t, Check(mod, pkgs), nil)
}

func assertViolations(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Check() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Check()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
