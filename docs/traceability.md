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
| FR-LOG-06 — 리플레이 결정론 속성 (300회, 입력 불변성 포함) | `core/replay_prop_test.go` — **T4에서 logd.Replay 배선, xfail 태그 제거 후 본 스위트 편입** | T2→T4 | green |
| FR-POL-03 — 병합 협소성 속성 (300회, 체인·교환·자기병합·승인 완화 금지) | `core/policy_prop_test.go` — **T6에서 policy.Merge 배선, xfail 태그 제거 후 본 스위트 편입** | T2→T6 | green |
| FR-POL-02 — 순수 평가 함수 (결정성·워크스페이스 경로 경계·egress 편입/거부 분리·깊이 거부) | `core/policy/policy_test.go` (TestEvaluatePure, TestEvaluateWorkspaceScope, TestEvaluateDepthDenial) | T6 | green |
| FR-POL-06 — 예산 초과 판정 (토큰/시간/깊이, 경계값 포함 7케이스) | `core/policy/policy_test.go` (TestExceededReason) | T6 | green |
| FR-POL-03 — 병합 단위 (교집합 정렬·최솟값·승인 강화·ID 계보) | `core/policy/policy_test.go` (TestMerge, TestIntersectDeterministic) | T6 | green |
| T6 재리뷰 — 워크스페이스 경로 탈출 차단 (POSIX 정규화 후 판정, 빈·상대 경로 거부, 빈 스코프 엔트리 무권한, 정규화 경로 반환) | `core/policy/policy_test.go` (TestEvaluateWorkspaceScope 탈출 3종+evil, TestEvaluateEmptyScopeEntryGrantsNothing, TestEvaluateReturnsNormalizedWorkspace) | T6 | green |
| FR-POL-01 — YAML 프로파일 파서 (strict+중복 키 거부, 다중 문서 거부(2차 Decode==io.EOF), 승인 명시·예산 3축 비음수·fs 절대경로 정규화) | `core/policy/parse_test.go` (거부 12종 + 유효·auto 명시) + boundarylint yaml import 한정 | T6 | green |
| T2 완료 기준 — 생성기가 스키마 유효·다양·결정적 입력을 실제 생성 | `core/propgen_test.go` (TestGenerator*) | T2 | green |
| FR-LOG-01 — append-only 저장소 수준 물리 차단 (직접 연결 UPDATE/DELETE 거부) | `seams/store/sqlite/store_test.go` (TestAppendOnlyTriggers) | T3 | green |
| FR-LOG-02 — 단일 writer seq 전순서 + mutation 비노출 (동시 제출·store 도달 순서, Reader 동적 타입의 Append/Close 미노출, 파일 직접 INSERT는 PK 최후 방어) | `core/logd/writer_test.go` (TestWriterSeqTotalOrder) + `seams/store/sqlite/store_test.go` (TestSingleWriterOverSQLite) + `seams/store/sqlite/bypass_test.go` (외부 패키지 assertion 검사) | T3 | green |
| T3 재리뷰 — contracts 위반 저장 거부, admission-only ctx, terminal 전파(원인 체인 포함), 백프레셔 cap 직접 단정, 특수문자 DSN | `core/logd/writer_test.go` (TestWriterRejectsContractViolations, TestSubmitAckDespiteCtxCancelAfterAdmission, TestWriterTerminalOnStoreFailure) + `seams/store/sqlite/store_test.go` (TestWeirdFilenames, TestLogPathEnforcesRedactionAndContract) | T3 | green |
| FR-LOG-07 — raw 원본 보존 (NULL/빈 base64 구분 왕복) | `seams/store/sqlite/store_test.go` (TestRoundTrip) | T3 | green |
| FR-LOG-08 — 기록 전 redaction (기본 패턴 + 설정 확장, payload·raw) | `core/logd/writer_test.go` (TestWriterRedaction) | T3 | green |
| FR-LOG-09 — 백프레셔 (큐 포화 시 입력 중단→재개, 수락분 전량 저장, 유실 0) | `core/logd/writer_test.go` (TestWriterBackpressure) | T3 | green |
| NFR-02 — 크래시 복구 (SIGKILL helper 후 마지막 ack seq까지 복원) | `seams/store/sqlite/crash_test.go` (TestCrashRecovery) | T3 | green |
| NFR-02/03 — WAL + synchronous=FULL 실적용, 재기동 seq 승계 | `seams/store/sqlite/store_test.go` (TestDurabilityPragmas, TestLastSeqAcrossReopen) | T3 | green |
| busy_timeout↔취소 충돌 회귀 (잠금 상태 50ms deadline 즉시 반환) + BUSY 재시도 | `seams/store/sqlite/store_test.go` (TestBusyReturnsPromptly…, TestBusyRetry…) | T3 | green |
| 외부 모듈 import 한정 (modernc→seams/store/sqlite, jsonschema→contracts/validate) | `tools/boundarylint/rules_test.go` (TestCheckExternalRestrictions) + `make lint` 실그래프 | T3 | green |
| NFR-04 — CGO_ENABLED=0 네이티브 smoke (race와 분리) | `make smoke` (CI 편입) | T3 | green |
| FR-LOG-03/04/10 — 모델 가시 히스토리·usage 프로젝션, 자식 중간 이벤트 제외·subagent/done만 진입, 재계산 가능성 | `core/logd/replay_test.go` (TestReplayMessagesProjection, TestReplayChildSpanConversationExcluded, TestReplayUsageAggregation, TestReplayDoesNotMutateInput) | T4 | green |
| FR-LOG-05 — 포크: 새 trace_id·원본 참조·포크 지점 상태 동일·원본 불변·불량 지점 거부 | `core/logd/replay_test.go` (TestForkPreservesStateAndOrigin, TestForkRejectsBadPoints) + `seams/store/sqlite/fork_test.go` (TestForkAcrossFiles — 실파일 독립 진행) | T4 | green |
| T4 재리뷰 — 포크 목적지 안전(자기 포크·비공백 목적지 거부, writer 루프 내 원자적 공백 확인, 정확한 atSeq 존재), usage 합산 fail-closed(overflow·음수 거부) | `core/logd/replay_test.go` (TestForkDestinationMustBeEmpty, TestForkRequiresExistingSeq, TestReplayUsageOverflowRejected) + `seams/store/sqlite/fork_test.go` (TestForkDestinationSafetyE2E — 공개 API 실파일) | T4 | green |
| T4 재재리뷰 — 포크 배치 저장소 수준 all-or-nothing (AppendBatch 트랜잭션) | `core/logd/replay_test.go` (TestForkAtomicOnStoreFailure) + `seams/store/sqlite/store_test.go` (TestAppendBatchAtomic — 실저장소 rollback) | T4 | green |
| FR-LOOP-02/03/04 — 훅 4지점 고정, 판정 3종, 충돌 해소(reject>rewrite>continue, 복수 rewrite 등록 순서) | `core/loop/hooks_test.go` (TestResolveDecisionsTable — 12조합, TestValidateDecision) + `core/loop/loop_test.go` (TestRegisterHookRejectsUnknownPoint, TestHooksAreIndependent) | T5 | green |
| FR-LOOP-05 — reject된 첫 step은 step 없는 durable turn | `core/loop/loop_test.go` (TestRejectedFirstStepLeavesDurableTurnWithoutSteps) + `seams/store/sqlite/loop_e2e_test.go` (실파일 reopen 후 재생) | T5 | green |
| FR-LOOP-01/06 — 상태 머신 경계·판정·사유의 이벤트 기록, 모델 요청의 로그 재구성(FR-LOG-03 구조 강제) | `core/loop/loop_test.go` (TestTurnFlowWithTools, TestPreToolRewriteApplied, TestPreToolRejectSkipsExecution, TestTurnStoppingRejectForcesAnotherStep, TestMaxStepsGuard, TestInvalidHookDecisionFails) | T5 | green |
| T5 재리뷰 — Writer 결속 프로젝션(혼합 불가), 오류·취소 경로 경계 보존, rewrite 전체 교체(strict decode) | `core/loop/loop_test.go` (TestModelCancellationKeepsBoundaries, TestSchemaViolatingUsageKeepsBoundaries, TestPreToolRewrite* 계열, TestPreStepRewriteIsFullReplacement, TestPostToolRewriteNonObjectFails…) | T5 | green |
| T5 재재리뷰 — hook/verdict durable 기록(훅의 ctx 취소에도 유실 0), 검증 후 기록(부분 기록 방지), 지점별 rewrite 대상 검증 | `core/loop/loop_test.go` (TestHookCancellationKeepsVerdictDurable, TestInvalidLaterDecisionPreventsPartialVerdictRecord, TestInvalidRewriteTargetPreventsVerdictRecord) | T5 | green |
| T5 payload 확정 (2026-08-17 [H] 승인) — 대화·툴 4종 + turn/step 경계 4종 폐쇄, tool/result status 판별, output 객체 정규화, args 정규화 | `contracts/validate/validate_test.go` (확정 유효 7종/위반 8종) + `core/propgen_test.go` (생성기 갱신) + codegen drift 게이트 | T5 | green |
| FR-ADP-01/02/03 — 어댑터 독립 실행 파일(NDJSON stdio), spawn/send/events/stop 최소 계약, ready·done MUST + 중간 이벤트 정규화 | `seams/subagent/subagent_test.go` (TestNullAdapterNormalization, TestStopPath) + `surfaces/hx/e2e_test.go` (실바이너리 관통) | T7 | green |
| §5.2 계약 위반 어댑터 거부 (비 NDJSON, raw 누락, 미지 kind, done 없는 종료) + 발신 명령 자체 검증 | `seams/subagent/subagent_test.go` (TestContractViolatingAdapterRejected) | T7 | green |
| FR-CLI-01/02 — hx run·hx replay(--to), stdout NDJSON/stderr 진단(FR-CLI-06), 재생 결정론·동일 상태 | `surfaces/hx/e2e_test.go` (TestRunReplayEndToEnd — replay 2회 동일, --to prefix) | T7 | green |
| FR-LOG-10 — 자식 중간 이벤트는 child span에만, 부모 히스토리에는 subagent/done 결과만 (E2E) | `surfaces/hx/e2e_test.go` (부모 Messages = subagent_result 1건 단정) | T7 | green |
| T8 준비 — 픽스처 비밀 검사 게이트 fail-closed 3분기 (무검출 0 / 검출 1 / 대상 부재·인자 누락·**실제 grep 실행 오류** 2) | `tools/fixturecheck/script_test.go` (TestSecretCheckGate — 7케이스, PATH 주입 가짜 grep으로 오류 분기 실증) | T8 | green |
| T8 준비 — 픽스처 매니페스트 게이트 (README 존재, NDJSON↔meta 대응, meta-only skip 계수 제외, 최소 15건) | `tools/fixturecheck/script_test.go` (TestManifestGate — 6그룹, `tools/check-fixture-manifest.sh` 실행) | T8 | green |
| FR-ADP-05 전제 — Claude Code·Codex 실출력 픽스처 15건(정상/툴 다수/승인 거부/오류/중단), meta-only skip 1건 | `contracts/fixtures/README.md` + `tools/check-fixture-secrets.sh contracts/fixtures` + `tools/check-fixture-manifest.sh contracts/fixtures 15` | T8 | green |
| T7 재리뷰 — §5.2 시퀀스 강제(첫 이벤트 ready, ready 중복·done 이후 출력·ready 전 이벤트 거부 + kill) | `seams/subagent/subagent_test.go` (TestSequenceViolationsRejected — 4형태) | T7 | green |
| T7 재리뷰 — 프로세스 수명 주기(단일 reap, Wait(ctx) deadline 준수, 위반 시 kill·drain, exit 오류 보존) | `seams/subagent/subagent_test.go` (TestWaitHonorsContextAfterDone, TestInvalidEventKillsLingeringProcess, TestAbnormalExitAfterDonePreserved) | T7 | green |
| T7 재리뷰 — hx run 원자적 세션 초기화(InitBatch, 두 번째 run 거부·로그 불변), replay 검증-후-출력(손상 로그 stdout 0바이트) | `surfaces/hx/e2e_test.go` (TestRunRefusesExistingSession, TestReplayCorruptedLogEmitsNothing) | T7 | green |
| T7 재재리뷰 — 종료 관측의 EOF 비종속(reaper + drain 유예), Spawn ctx 취소의 그룹 kill, 공백 줄 §5.2 위반 | `seams/subagent/subagent_test.go` (TestExitObservationIndependentOfStdoutEOF, TestSpawnContextCancelKillsGroup, TestBlankLinesAreViolations — 3위치) | T7 | green |
| T7 재재재리뷰 — exec.Cmd 자원 소유(watchCtx·파이프 누수 소멸: 완료 후 파이프 closure, 반복 spawn goroutine 비증가, 실패 경로 정리), 리더 종료 시 그룹 즉시 종료 + 진짜 EOF drain(유예 fail-open 소멸) | `seams/subagent/subagent_test.go` (TestPipesClosedAfterCompletion, TestNoGoroutineAccumulationAcrossSpawns, TestSpawnSendFailureCleansUp, TestLeaderExitTerminatesRemainingGroup) | T7 | green |
| FR-ADP-02 — Claude Code command 수신(task/stop), `message`는 단발 `-p`·세션 저장 배제에 따른 명시적 fail-closed 제약(done/error 후 종료) | `seams/subagent/claudecode/adapter_test.go` (TestAdapterStopKillsClaudeAndSynthesizesStopped, TestAdapterEmitsDistinctDoneForFailuresAfterReady/message_unsupported) + `docs/t9-adapter-contract-proposal.md` §7.5 | T9 | green |
| FR-ADP-03 — Claude Code 독립 어댑터의 ready/done MUST, init-first, stop·오류 경로 terminal 보존 | `seams/subagent/claudecode/adapter_test.go` (TestAdapterExecutableReplaysAllClaudeFixtures, TestAdapterRejectsNativeEventBeforeInitWithoutBrokenOutput, TestAdapterEmitsDistinctDoneForFailuresAfterReady, TestAdapterStopKillsClaudeAndSynthesizesStopped) | T9 | green |
| FR-ADP-04 — Claude native NDJSON 및 PreToolUse 훅 원본 raw 보존(1원본→N이벤트 동일 raw 포함) | `seams/subagent/claudecode/parse_fail_test.go` (TestSameRawAttachedToEachEvent) + `seams/subagent/claudecode/adapter_test.go` (TestApprovalHandshakeAllowAndDenyPreservesRaw) | T9 | green |
| FR-ADP-05 — Claude stream-json 정규화·픽스처 스냅샷, 중간 이벤트 유실 0 | `seams/subagent/claudecode/golden_test.go` (TestGoldenClaudeFixtures, TestNoIntermediateEventLoss) + `seams/subagent/claudecode/adapter_test.go` (TestAdapterExecutableReplaysAllClaudeFixtures) | T9 | green |
| FR-ADP-06 — Claude 관측 가능 등급 `observable` | `seams/subagent/claudecode/golden_test.go` (8개 골든의 ready.grade) + `seams/subagent/claudecode/adapter_test.go` (TestAdapterExecutableReplaysAllClaudeFixtures) | T9 | green |
| FR-ADP-07 — result.usage 1회 보고, 입력 3항 checked addition, 부재 폴백·손상 fail-closed | `seams/subagent/claudecode/parse_fail_test.go` (TestUsageAbsentIsOmittedNotError, TestCacheFieldsDefaultToZero, TestFailClosedCases의 usage 3종) + 골든 8건 | T9 | green |
| FR-POL-05 — 승인 handshake(명시 auto만 자동 허용, manual nil 기본 deny, durable policy/decision 후 응답, pump 비블로킹, 미상관·중복·빈 reason·기록 실패·stop·취소 fail-closed) | `seams/subagent/subagent_test.go` (TestApprovalPolicyModesAndDurableAttribution, TestApprovalDecisionDoesNotBlockPump, TestApprovalEmptyDenyReasonIsFatalAfterForcedDeny, TestApprovalRecordFailureSendsDenyBeforeTermination) + `seams/subagent/claudecode/adapter_test.go` (TestApprovalHandshakeAllowAndDenyPreservesRaw, TestApprovalResponseCorrelationViolations, TestStopDeniesPendingHookBeforeNativeTermination, TestContextCancellationCleansPendingHookViaProcessDone) | T9 | green |
| FR-POL-05 실증 — 실 claude 세션에서 훅 발화·부모 판정이 툴 실행을 게이트 (deny=미생성 / allow=생성) | `seams/subagent/claudecode/smoke_test.go` (TestSmokeApprovalHandshake, `-tags smoke`, [H] 사람 실행) + `docs/t9-smoke-result.md` | T9 | green (2026-08-19, claude 2.1.235) |
| FR-POL-05 격리 — 고정 플래그 `--setting-sources project,local`이 사용자 설정을 실제로 배제(대조군 대비) | `seams/subagent/claudecode/smoke_test.go` (TestSmokeUserSettingIsolation, `-tags smoke`; 자격증명·API 호출 없음) + `docs/t9-smoke-result.md` §확인점 1 | T9 | green (2026-08-19, claude 2.1.235) |
