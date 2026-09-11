# T17 계약 회신 — `hx run` 접수 표면의 구현 사실과 개정 제안

작성 2026-09-11, JANUS T17 작업 세션. 수신: Rhizome 계약 소유 세션
(`../Rhizome/docs/janus-execution-contract.md` §3·§8-①, 결정 시트 2·3번).

공개 프로토콜 스키마는 Rhizome이 임시 소유하므로(결정 1번) JANUS는 계약
파일을 만들지 않았다. 이 문서는 (1) 계약이 JANUS 결정 사항으로 위임한
물리 형식의 확정 내용과 (2) 계약 초안과 어긋나거나 초안에 없어 **개정
제안으로 회신하는** 항목을 구분해 기록한다.

## 1. JANUS 결정 사항 확정 (계약이 위임한 부분)

- **keyhash**: `hex(sha256(uvarint(len(scope)) ‖ scope ‖ uvarint(len(key)) ‖ key))`.
  계약 §3.3은 key만 length-prefix했으나 scope도 함께 length-prefix한다
  (`("ab","c")`/`("a","bc")` 경계 모호성 차단). 구현: `seams/accept`.
- **세션 경로**: `<accept-root>/<keyhash>/session.sqlite`. 접수 레지스트리
  root와 trusted session root를 한 디렉터리로 통합했다 — claim.json·마커·
  세션 파일이 같은 keyhash 디렉터리에 있어 tombstone 관측이 단순해진다.
- **claim 물리 형식**: `claim.json`(임시 파일 fsync 후 link(2)로 배타 공개,
  이후 불변·영구), `db-initialized` 마커(session DB 초기화 완료),
  `launch` 마커(외부 효과 직전, key당 1회). claim.json이 삭제되지 않는
  것 자체가 tombstone이다: session.sqlite가 사라져도 같은 key는
  `ACCEPTANCE_UNKNOWN`으로 끝나며 재초기화·재spawn하지 않는다.
- **profile_hash(요청 pin)**: `sha256(uvarint(len(b₀))‖b₀‖…)` — b₀는
  `--profile` 파일 원문 바이트, 이후 `--overlay` 순서대로. 실행 시
  재계산해 불일치면 `POLICY_CHANGED`.
- **policy_hash(실효 정책 fingerprint)**: `hx dump-config --profile …
  --overlay … --workspace <workspace_ref>` stdout 바이트의 sha256과 정의상
  동일. Rhizome은 사전 dump-config 출력을 hash해 같은 값을 예측·대조할
  수 있고, 접수 응답·binding 이벤트의 `policy_hash`가 이 값이다.
- **acceptance_seq = 1 고정**: 접수 binding은 배타적 초기화 배치의 첫
  이벤트(session/start)의 payload `execution_binding`에 실린다. 필드:
  operation_id, scope, idempotency_key, request_fingerprint, policy_hash,
  profile_id, adapter_id, workspace_ref, rhizome_execution_id, mission_id,
  correlation_id. `hx replay` NDJSON으로 공개 관측 가능.
- **실효 budget**: 요청 budget이 병합 정책을 축별로 초과하면
  `BUDGET_INVALID`, 통과하면 요청 값이 실효 budget이다(좁힘만 가능).

## 2. 계약 개정 제안 (Rhizome 승인 필요)

1. **request.json에 `scope` 필드 추가**. §3.3의 scope(설치/소유자·executor
   namespace)가 §3.1 필드 목록에 없다. JANUS 구현은 `scope`를 필수
   문자열로 요구한다.
2. **task_ref의 v1 형태**: `{"instruction": "<어댑터 지시>"}` 객체로
   한정 구현했다. 참조형 task_ref는 후속 계약.
3. **오류 코드 추가**: `POLICY_DENIED`(정책 평가의 명시 거부 — 워크스페이스
   스코프 밖 등. `POLICY_UNMAPPABLE`은 Rhizome 쪽 변환 실패의 의미라
   재사용하지 않았다), `LAUNCH_FAILED`(접수 후 실행 조립 실패 — terminal
   메시지에 실리며 접수 사실은 유지).
4. **status 어휘**: 접수 응답 `accepted` 외에 `initializing`(claim durable,
   DB 초기화 미완 — 재조회 대상), `unknown`(claim은 있으나 세션 파일
   부재 = tombstone), `rejected`(접수 전 거부), `terminal`(실행 종료
   보고)을 stdout 제어 메시지로 쓴다. 계약 §4 lookup 상태 어휘의 부분
   집합이다.
5. **응답 확장 필드**: `launch_claimed`(launch claim 존재 여부 —
   "DB 초기화 완료"와 "실행 시작"의 구분), `duplicate`(무spawn 재조회
   응답 표시), terminal 메시지의 `done {status,result}`·`last_seq`.
6. **엄격 해석**: v1 request는 미지 필드를 `UNSUPPORTED_CONTRACT`로
   거부한다(DisallowUnknownFields). 필드 추가는 버전 상향으로.
7. **CLI 형태**: `--adapter`는 생산 경로에서 받지 않는다 — 어댑터는
   `request.adapter_id`(코드 고정 허용 목록 claudecode|codex)로 지정하고,
   실행 파일·이미지 digest 등 호스트 자산은 운영자 소유 `--world-config`
   JSON으로 분리했다. `--workspace`도 생산 경로에서 무시되며
   `request.workspace_ref`가 유일한 입력이다. `--session`은 선택이고,
   주어지면 keyhash 정본 경로와 일치해야 한다(불일치 = `KEY_CONFLICT`).
8. **`--approval-endpoint`는 미구현**: T18(원격 ApprovalDecider)에서
   구현한다. 그때까지 생산 경로의 decider는 DenyAll 고정이다 — 자동
   allow 경로 없음.
9. **재요청의 정책 재검증**: 같은 key+fingerprint 재조회라도 현재 병합
   정책의 policy_hash가 기존 접수의 값과 다르면 `POLICY_CHANGED`로
   거부한다(과거 접수를 새 정책으로 위장 재조회하는 경로 차단).

## 3. 검증 상태

- macOS `make ci`: 접수 파이프라인 완료 기준 (a) 멱등/KEY_CONFLICT,
  (b) 동시 단일 launch, (c) tombstone, (d) POLICY_CHANGED —
  `surfaces/hx/run_production_test.go`, `seams/accept/accept_test.go`.
- Linux CI 게이트: `TestProductionRunClaudeIntegration`
  (`make t15-integration`, 5-attempt matrix) — 실 rootless Podman world에서
  접수→launch claim→world spawn→tokenless 인증 실패(error done)와 무spawn
  중복 접수를 관통 검증.
- 실 토큰 smoke는 [H] 직접 수행(결정 10번) — T15 runbook 경로 재사용.
