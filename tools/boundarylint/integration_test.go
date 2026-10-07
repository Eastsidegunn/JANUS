package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// 실제 go list를 통한 통합 테스트: 임시 모듈에 T0.1 리뷰가 실증한
// 우회 시나리오들을 재현하고, 린터가 전부 잡는지 확인한다.
func TestIntegrationBlindSpots(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":                        "module example.com/hx\n\ngo 1.26\n",
		"core/core.go":                  "package core\n",
		"core/world/worldtest/doc.go":   "package worldtest\n",
		"surfaces/doc.go":               "package surfaces\n\nimport _ \"example.com/hx/core/world/worldtest\"\n",
		"collector/doc.go":              "package collector\n",
		"seams/adapter/doc.go":          "package adapter\n",
		"seams/adapter/adapter_test.go": "package adapter\n\nimport _ \"example.com/hx/core/world/worldtest\"\n",
		// 발견 1: 테스트 파일에서만 일어나는 위반
		"core/core_test.go": "package core\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/hx/surfaces\"\n)\n\nfunc TestX(t *testing.T) {}\n",
		// 발견 1 확장: 특정 GOOS에서만 빌드되는 파일의 위반
		"core/linux_only.go": "//go:build linux\n\npackage core\n\nimport _ \"example.com/hx/collector\"\n",
		// 발견 2: 미분류 최상위 패키지
		"rogue/doc.go": "package rogue\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	module, err := modulePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/hx" {
		t.Fatalf("modulePath = %q", module)
	}

	var pkgs []Pkg
	for _, goos := range sweepGOOS {
		got, err := listPackages(dir, goos)
		if err != nil {
			t.Fatalf("listPackages(GOOS=%s): %v", goos, err)
		}
		pkgs = append(pkgs, got...)
	}
	got := Check(module, pkgs)
	want := []string{
		"core → collector", // linux 전용 파일 — GOOS 순회 없이는 darwin에서 못 잡는다
		"core → surfaces",  // 테스트 전용 import
		"surfaces → core/world/worldtest (worldtest는 _test.go에서만 import 가능)",
		"미분류 최상위 디렉토리: rogue (허용: contracts|core|seams|collector|surfaces|tools)",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("위반 누락: %q (전체: %v)", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Check() = %v, want %v", got, want)
	}

	// darwin 단독 그래프에서는 linux 전용 위반이 안 보여야 한다 —
	// GOOS 순회가 실제로 사각지대를 메우고 있음을 확인.
	darwinOnly, err := listPackages(dir, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(Check(module, darwinOnly), "core → collector") {
		t.Error("darwin 단독 그래프에 linux 전용 위반이 보임 — 테스트 전제가 깨짐")
	}
}
