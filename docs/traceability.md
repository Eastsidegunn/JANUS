# 추적 매트릭스 — FR ID → 테스트

각 태스크 완료 시 해당 행을 추가한다. 테스트 없는 FR 구현 주장은 무효다.

| FR / 불변식 | 테스트 | 태스크 | 상태 |
|---|---|---|---|
| §3.1 의존 방향 (불변식: 의존은 아래로만, seam 수평 금지, collector-core 분리) | `tools/boundarylint/rules_test.go` + `make lint` 실그래프 검사 | T0 | green |
| §3.1 경계 린트 사각지대 (테스트 import, GOOS build tag, 미분류 패키지) | `tools/boundarylint/rules_test.go` (TestCheckTestImports, TestCheckRoguePackage, TestCheckDedup) + `tools/boundarylint/integration_test.go` | T0.1 | green |
| §5.1 이벤트 스키마 — 유효 샘플 수용·위반 샘플 거부 | `contracts/validate/validate_test.go` (TestValidateRecordValid/Invalid) | T1 | green |
| §5.2 와이어 프로토콜 — 방향별 검증(command/event), raw 필수, 합성 이벤트 `""` | `contracts/validate/validate_test.go` (TestValidateCommand, TestValidateEvent) | T1 | green |
| FR-OBS-01 — OTel all-zero trace/span ID 거부 | `contracts/validate/validate_test.go` (TestValidateRecordInvalid: all-zero 케이스) | T1 | green |
| codegen 산출 타입 ↔ 스키마 정합 (T1 완료 기준 a) | `contracts/validate/validate_test.go` (TestGenTypesRoundTrip) | T1 | green |
| schemagen fail-closed·결정성·병합 규칙 | `tools/schemagen/gen_test.go` | T1 | green |
| codegen drift 게이트 (미추적 신규 파일 포함) | `tools/schemagen/drift_test.go` + `make codegen-drift` (CI 편입) | T1 | green |
| donePayload 거울 정의 동일성 | `contracts/schema_test.go` | T1 | green |
