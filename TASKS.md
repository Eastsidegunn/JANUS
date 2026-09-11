# TASKS.md — HX 구현 작업 큐

규칙: 위에서 아래로, 한 번에 하나. 각 태스크의 "완료 기준"이 CI green이 되어야 다음으로 넘어간다.
[H] 표시는 사람이 직접 하거나 사람 리뷰가 필수인 태스크.

---

## T0. 저장소 강제 장치 [H: 설정 리뷰]
- 내용: 모노레포 초기화(contracts/core/seams/collector/surfaces), 경계 린트 설정(의존 방향 규칙 §3.1, seam 수평 import 금지), CI 파이프라인(lint→test→fixtures), Makefile.
- 대상: NFR 전반, 불변식 5
- 완료 기준: 고의로 위반하는 더미 import를 넣으면 CI가 실패하고, 제거하면 통과한다.

## T1. contracts 스키마 + codegen [H: 스키마 최종 리뷰 필수]
- 내용: `events.schema.json`(§5.1), `wire.schema.json`(§5.2) 작성. JSON Schema → Go 타입 codegen 파이프라인. kind 어휘 전체 포함.
- 대상: FR-LOG 스키마, FR-ADP-01, §5 전체
- 완료 기준: codegen 산출 타입으로 §5.1 예시 이벤트들이 파싱·검증된다. 스키마 위반 샘플은 거부된다.
- 주의: 이 저장소에서 가장 되돌리기 비싼 산출물. 사람 리뷰 없이 머지 금지.

## T2. 속성 테스트 골격 (실패 상태로 커밋)
- 내용: FR-LOG-06(리플레이 결정론), FR-POL-03(병합은 좁아지기만)의 속성 테스트를 구현 없이 먼저 작성. CI에서 expected-fail로 표시.
- 완료 기준: 두 테스트가 존재하고, 임의 입력 생성기가 이벤트 시퀀스/프로파일 조합을 실제로 생성한다.

## T3. logd — 단일 writer + append-only 저장소
- 내용: SQLite WAL, events 테이블(§5.1), UPDATE/DELETE 차단 트리거, writer의 seq 발급, fsync 내구성, 백프레셔 인터페이스.
- 대상: FR-LOG-01/02/07/08/09, NFR-02/03
- 완료 기준: append-only 트리거 테스트, 동시 쓰기 시도 시 writer 경유 강제 테스트, redaction 기본 패턴 테스트 green.

## T4. 프로젝션 + 리플레이 + 포크
- 내용: deriveMessages 등 프로젝션, `replay(events)` 재계산, seq 지점 포크(새 trace_id, 원본 참조).
- 대상: FR-LOG-03/04/05/06
- 완료 기준: T2의 리플레이 결정론 속성 테스트가 expected-fail에서 green으로 전환. 포크 후 원본 불변 테스트 green.

## T5. loop — turn/step 상태 머신 + 훅 4지점
- 내용: 고정 상태 머신, pre_step/pre_tool/post_tool/turn_stopping, 판정 모델(continue|rewrite|reject)과 충돌 해소(reject>rewrite>continue), 판정의 이벤트 기록.
- 대상: FR-LOOP 전체
- 완료 기준: 훅 판정 조합 테이블 테스트, reject된 첫 step이 step 없는 durable turn으로 남는 테스트 green.

## T6. policy — 프로파일 파서 + 순수 평가 함수
- 내용: YAML 프로파일(§FR-POL-01 필드), (프로파일, spawn 요청)→거부|샌드박스 설정, 교집합/최솟값 병합.
- 대상: FR-POL-01/02/03/06
- 완료 기준: T2의 병합 협소성 속성 테스트 green. 예산 초과 판정 단위 테스트 green.

## T7. null 어댑터 + 워킹 스켈레톤
- 내용: 와이어 프로토콜(§5.2)을 말하는 스크립트 기반 가짜 어댑터. `hx run`→spawn→NDJSON 파싱→writer→child span→`hx replay` 관통.
- 대상: FR-ADP-01/02/03, FR-CLI-01/02, FR-LOG-10
- 완료 기준: null 어댑터 세션의 E2E 테스트 — run 후 replay가 동일 상태, 자식 중간 이벤트가 부모 모델 히스토리에 미포함.

## T8. 픽스처 녹화 [H: 사람이 실행]
- 내용: Claude Code, Codex의 실제 출력 녹화 15~20 시나리오(정상/툴 다수/승인 요청/에러/중단). `contracts/fixtures/`에 커밋.
- 대상: FR-ADP-05의 전제
- 주의: 실 자격증명 필요. 에이전트에게 위임하지 않는다.

## T9. Claude Code 어댑터
- 내용: stream-json → 정규화 이벤트 변환, raw passthrough, usage 보고, 관측 가능 등급 선언, 승인 요청 승격.
- 대상: FR-ADP-03/04/05/06/07, FR-POL-05
- 완료 기준: T8 픽스처 전체에 대한 스냅샷 테스트 green.

## T10. world local 백엔드 — 컨테이너 실행
- 내용: OCI 컨테이너(rootless) spawn, 워크스페이스 overlayfs 마운트, egress 기본 차단 + 프록시, 단기 자격증명 주입, 어댑터는 호스트 측. 실행 환경 메타데이터의 spawn 이벤트 기록.
- 대상: FR-SBX 전체, FR-ADP-10
- 완료 기준: allowlist 밖 도메인 접근 차단 통합 테스트(§8-4), 컨테이너 내부에서 어댑터 프로세스 접근 불가 확인.

## T11. collector — fsdiff + egress
- 내용: overlayfs upper 기반 변경 파일 목록 → `collector/fs_changed`, 프록시 로그 → `collector/egress`(메타데이터만). span 귀속, writer 경유.
- 대상: FR-COL-01/02/03/05/06
- 완료 기준: 에이전트가 보고하지 않은 파일 변경이 collector 이벤트로 잡히는 통합 테스트 green.

## T12. hx audit — 대조 리포트
- 내용: 의도 평면 vs 효과 평면 대조, 불일치 3분류(보고-무관측/관측-무보고/일치), span·비용 질의.
- 대상: FR-AUD 전체, FR-CLI-04
- 완료 기준: 인위적 불일치 시나리오(null 어댑터가 거짓 보고) 검출 테스트 green — §8-3.

## T13. 확장 패스스루 — 프로비저닝 단계
- 내용: extensions 선언 파싱(해시 고정 검증), 프로비저닝/실행 프로파일 분리, 설치 세트의 spawn 이벤트 기록, 콘텐츠 주소 캐시.
- 대상: FR-EXT 전체
- 완료 기준: §8-8 — 프로비저닝 중 레지스트리 접근 성공, 실행 중 동일 도메인 차단, 확장 세트 기록 확인.

## T14. OTel export + Codex 어댑터 + dump-config
- 내용: trace/span export(FR-OBS-01/02), Codex 어댑터(T8 픽스처 기반), `hx dump-config`.
- 완료 기준: Jaeger 렌더링 확인(§8-7), Codex 픽스처 테스트 green.

---

## T15. Claude Code 에이전트의 sandbox 실행
- 내용: 어댑터와 에이전트 본체를 분리해 `claude`를 컨테이너 안에서 실행. 호스트 어댑터는 world process broker로 stdio를 중계한다.
- 대상: FR-ADP-10, FR-SBX-01(Claude Code 경로), FR-SBX-04
- **착수 조건([H])**: 컨테이너 안에서 단기·스코프 한정 자격증명으로 `claude`가 동작하는지 실측. 근거는 docs/t10-scope-determination.md §6.
- 2026-08-31: Bedrock·Vertex 접근 없음 확인 — 3P STS 경로는 이 환경에서 불가. 남은 실측 후보는 OAuth access token 단독 주입(동 문서 §6의 (i)/(ii) 분해).
- 2026-09-02: (i)·(ii) 모두 성립 — **착수 조건 충족**. 잔여 판단(스코프 축소 미충족)은 동 문서 §6 참조.
- 2026-09-06: 구현·CI 게이트 완료. 실 토큰 컨테이너 세션은 문서화된 잔여(B결정) — docs/v0.1-release-acceptance.md §2 잔여, docs/traceability.md.
- 완료 기준: Claude Code spawn에서 에이전트 프로세스가 컨테이너 내부에 존재하고, 워크스페이스 변경이 overlay upper에 잡히며, egress가 프록시를 경유함을 통합 테스트로 확인.

## T16. Codex 어댑터 완성 (§8-2 Codex 실 세션)
- 내용: Codex 독립 실행파일(cmd/), 비대화형 승인 경로 설계, [H] 실 codex 세션 smoke.
- 대상: FR-ADP-09, §8-2 (Codex 부분)
- 완료 기준: 실 codex 세션에서 자식 툴 콜이 child span으로 기록되고 승인 게이트가 동작. 픽스처 밖 동작을 [H] 실측(T9 교훈).
- 근거: docs/v0.1-release-acceptance.md "Codex 실 세션 — 후속 종결 조건".

## T17. `hx run` 생산 샌드박스 배선 + scoped key/claim/tombstone
- 내용: `hx run`이 production world(podman)·승인된 어댑터·`--profile/--overlay` 정책·세 축 budget을 실제로 조립한다(`startProductionWorld`의 프로덕션 호출자). 샌드박스 없는 시작 경로는 만들지 않는다. scoped idempotency key의 배타 점유, **외부 효과 전** durable 접수/launch claim 기록, 삭제된 세션 key 재사용 차단(tombstone), 동시 요청 시 단일 launch 보장. 접수 응답은 NDJSON stdout(`accepted`는 binding durable 후에만, 실행 완료 의미 아님)으로 `session_ref`/`trace_id`/`policy_hash`/`request_fingerprint`/`acceptance_seq`를 제공. 사전 `dump-config`와 실행 시 재병합의 정책 fingerprint 일치 검증, 불일치 시 `POLICY_CHANGED` 거부.
- 대상: FR-CLI-01/05/06, FR-SBX-01~06, FR-POL-01/02/03/04, FR-LOG-01(durable 접수)
- 근거: ../Rhizome/docs/scp-rhz032-decision-sheet.md 결정 2·3번([H] 2026-09-10, T16+ 착수 판단 겸함), ../Rhizome/docs/janus-execution-contract.md §3·§8-①. 물리 형식(claim/tombstone 저장)은 JANUS 결정 사항.
- 완료 기준: (a) 같은 key+fingerprint 재요청이 새 spawn 없이 동일 세션을 반환하고, 같은 key+다른 fingerprint가 `KEY_CONFLICT`로 거부되는 테스트, (b) 동시 두 요청에서 launch가 정확히 하나인 테스트, (c) 삭제된 세션 key 재사용이 tombstone으로 거부되는 테스트, (d) 정책 파일 변조 시 `POLICY_CHANGED` 거부 테스트 — macOS `make ci` green. world 실행 게이트는 Linux CI(push) green 필수, 실 토큰 smoke는 [H] 직접 수행(결정 10번).
- 주의: 공개 프로토콜 스키마는 Rhizome 임시 소유(결정 1번) — JANUS에 새 계약 파일을 만들지 않고, 스키마 필요 사항은 Rhizome 쪽 제안으로 회신한다. contracts/ 수정 금지 유지.

## T18. 원격 ApprovalDecider — 로컬 Unix 소켓 NDJSON relay
- 내용: `policy.ApprovalDecider` 구현체 추가(DenyAll 외 최초의 운영 decider). 기존 approvalCoordinator의 durable request→Decide()→durable response 순서·deny 규칙(mismatch/timeout/lease 종료 = durable deny)을 그대로 보존한다. 소켓 소유권/권한·peer 검증, 요청 범위 상관(trace_id·span_id·request_id), deadline/lease, 같은 response_id 중복 응답 거부(다른 내용은 conflict), deny reason 필수. 원문 tool args는 relay로 내보내지 않는다 — digest와 안전 요약만(계약 §6.1). 결정 전 crash 후 재조회(전달 이력 없는 durable deny 관측) 지원. `hx run`에서 decider 선택 가능(기본은 여전히 DenyAll — 명시 opt-in). 자동 allow를 표현할 수 있는 어떤 경로도 만들지 않는다. 신원 미검증 기간에는 승인 기록에 `unverified-local-operator` 표시.
- 대상: FR-POL-05, FR-LOG-01
- 근거: 결정 시트 7번([H] 2026-09-10), 계약 §6·§8-②.
- 완료 기준: relay decider의 (a) 정상 allow/deny 왕복, (b) deadline 경과 시 durable deny, (c) 같은 response_id 재전달 멱등·다른 내용 conflict, (d) request scope mismatch 거부, (e) 원문 args 미노출 검증 테스트 — `make ci` green. 기존 승인 배관 속성/단위 테스트 무손상.

## T19. `hx stop` CLI — 소유 프로세스 제어 표면
- 내용: 세션 소유 프로세스의 제어 표면으로 중단 요청을 전달한다(DB writer 우회 금지, bare PID kill 불채택). 중단 **접수**(`stop_accepted {stop_id, request_ref}` 또는 `already_terminal`)와 실제 **종료 사실**(`subagent/done status: stopped` + 세션 종료·collector 정리)은 별개 조회. reason enum은 `user|budget_exceeded|policy|parent_done`만 허용하고 권한 분리(사람=`user`, budget/policy는 로그로 입증 가능한 조건 한정) — 사유 위장 차단. 같은 stop_id+내용은 멱등, 다른 내용은 conflict. 전송 timeout ≠ cancelled.
- 대상: FR-POL-06, FR-ADP-02(stop), FR-CLI-06, §5.2 stop 메시지
- 근거: 결정 시트 8번([H] 2026-09-10), 계약 §7·§8-③.
- 완료 기준: (a) 실행 중 세션에 stop 요청 → stop_accepted 후 `subagent/done status: stopped`와 세션 종료가 로그로 확인되는 테스트, (b) 같은 stop_id 재요청 멱등 테스트, (c) 종료된 세션에 already_terminal 응답 테스트, (d) reason 위장(비인가 budget/policy 사유) 거부 테스트 — `make ci` green.

## 이후 (T19+, 착수 전 사람 판단 필요)
- pi / OpenClaw / 사내 에이전트 어댑터 (contracts 안정성의 성적표)
- exec 감사(eBPF), Postgres store, 규칙 엔진 — 전부 명세 부록 A의 미결 확정 후.
