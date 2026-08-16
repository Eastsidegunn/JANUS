# 추적 매트릭스 — FR ID → 테스트

각 태스크 완료 시 해당 행을 추가한다. 테스트 없는 FR 구현 주장은 무효다.

| FR / 불변식 | 테스트 | 태스크 | 상태 |
|---|---|---|---|
| §3.1 의존 방향 (불변식: 의존은 아래로만, seam 수평 금지, collector-core 분리) | `tools/boundarylint/rules_test.go` + `make lint` 실그래프 검사 | T0 | green |
| §3.1 경계 린트 사각지대 (테스트 import, GOOS build tag, 미분류 패키지) | `tools/boundarylint/rules_test.go` (TestCheckTestImports, TestCheckRoguePackage, TestCheckDedup) + `tools/boundarylint/integration_test.go` | T0.1 | green |
