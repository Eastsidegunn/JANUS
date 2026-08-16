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
| FR-LOG-06 — 리플레이 결정론 속성 (300회) | `core/replay_prop_test.go` (xfail 태그, T4에서 배선·편입) | T2 | **expected-fail** |
| FR-POL-03 — 병합 협소성 속성 (300회, 체인·교환·자기병합 포함) | `core/policy_prop_test.go` (xfail 태그, T6에서 배선·편입) | T2 | **expected-fail** |
| T2 완료 기준 — 생성기가 스키마 유효·다양·결정적 입력을 실제 생성 | `core/propgen_test.go` (TestGenerator*) | T2 | green |
| FR-LOG-01 — append-only 저장소 수준 물리 차단 (직접 연결 UPDATE/DELETE 거부) | `seams/store/sqlite/store_test.go` (TestAppendOnlyTriggers) | T3 | green |
| FR-LOG-02 — 단일 writer seq 전순서 (동시 제출·store 도달 순서·우회 seq 거부) | `core/logd/writer_test.go` (TestWriterSeqTotalOrder) + `seams/store/sqlite/store_test.go` (TestSingleWriterOverSQLite) | T3 | green |
| FR-LOG-07 — raw 원본 보존 (NULL/빈 base64 구분 왕복) | `seams/store/sqlite/store_test.go` (TestRoundTrip) | T3 | green |
| FR-LOG-08 — 기록 전 redaction (기본 패턴 + 설정 확장, payload·raw) | `core/logd/writer_test.go` (TestWriterRedaction) | T3 | green |
| FR-LOG-09 — 백프레셔 (큐 포화 시 입력 중단→재개, 수락분 전량 저장, 유실 0) | `core/logd/writer_test.go` (TestWriterBackpressure) | T3 | green |
| NFR-02 — 크래시 복구 (SIGKILL helper 후 마지막 ack seq까지 복원) | `seams/store/sqlite/crash_test.go` (TestCrashRecovery) | T3 | green |
| NFR-02/03 — WAL + synchronous=FULL 실적용, 재기동 seq 승계 | `seams/store/sqlite/store_test.go` (TestDurabilityPragmas, TestLastSeqAcrossReopen) | T3 | green |
| busy_timeout↔취소 충돌 회귀 (잠금 상태 50ms deadline 즉시 반환) + BUSY 재시도 | `seams/store/sqlite/store_test.go` (TestBusyReturnsPromptly…, TestBusyRetry…) | T3 | green |
| 외부 모듈 import 한정 (modernc→seams/store/sqlite, jsonschema→contracts/validate) | `tools/boundarylint/rules_test.go` (TestCheckExternalRestrictions) + `make lint` 실그래프 | T3 | green |
| NFR-04 — CGO_ENABLED=0 네이티브 smoke (race와 분리) | `make smoke` (CI 편입) | T3 | green |
