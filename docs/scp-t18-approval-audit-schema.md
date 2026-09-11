# SCP-T18-001 — 승인 감사 필드의 이벤트 스키마 변경 제안

작성 2026-09-12, T18 작업 세션. 차단 기록: BLOCKED.md "T18 SCP-T18-001".

> **결정: 3안 승인** (2026-09-12, [H]) — policyDecisionPayload optional
> 확장 + relay 경로의 필드 기록 테스트 의무화. `decision_source`는 enum
> `local|relay|forced` 유지. 스키마 수정 커밋은 T1 조항대로 [H] 리뷰 후
> 머지하며, 필드명 확정본은 Rhizome 리뷰어에게 사본 통지한다.

## 1. 왜 필요한가

T18 원격 ApprovalDecider(계약 §6, 결정 시트 7번)의 재조회 의미론은
"Rhizome이 전달 이력 없이도 JANUS의 durable 결정(decided/expired)을
관측"할 수 있어야 한다. T17-1에서 [H]가 비준한 조건 — **로그 밖 원본
기록의 예외는 accept 레지스트리 하나로 한정** — 에 따라, relay가 재조회에
답하는 durable 상태의 원본은 세션 이벤트 로그여야 하고 relay 서버 상태는
그로부터 재계산 가능해야 한다.

그런데 현재 durable 승인 판정 이벤트인 `policy/decision`의 payload는
`{decision, profile_id, reason}` (additionalProperties: false)뿐이다:

- **request_id 상관 필드가 없다** — 어떤 approval_request에 대한 판정인지
  span·순서로만 묶인다. request_key 기반 재조회의 로그 근거가 없다.
- 계약 §6.1의 감사 필드(operation_id, response_id, actor_ref,
  human_intent_id, correlation_id)를 보존할 자리가 없다 — "외부 request
  scope·actor 연결은 JANUS 승인 감사 기록에 보존되어야 한다"(§6.1)와
  `unverified-local-operator` 표시(결정 시트 7번 조건)를 이행할 수 없다.
- response_id 멱등/conflict 판정(§6.3)도 로그에서 재계산할 수 없다.

## 2. 현황 사실

- `policy/decision`의 유일한 생산자는 `seams/subagent/approval.go`의
  approvalCoordinator다(승인 판정 전용). 다른 용도 없음.
- `subagent/approval_response` 이벤트 kind는 존재하지 않는다.
  `ApprovalResponsePayload`는 wire.schema.json의 parent→adapter 명령이다.
- 기존 녹화 세션·픽스처의 policy/decision 이벤트는
  `{decision, profile_id, reason}`만 갖는다 — 소급 무효화는 불가하다.

## 3. 제안: policyDecisionPayload의 optional 확장 (권고안)

`contracts/events.schema.json`의 `policyDecisionPayload`에 **선택
(optional) 필드**를 추가한다. required는 기존 그대로
`[decision, profile_id]`, additionalProperties: false 유지:

| 필드 | 타입 | 의미 |
|---|---|---|
| `request_id` | string, minLength 1 | 대응 `subagent/approval_request`의 request_id — request_key 상관의 로그 근거 |
| `response_id` | string, minLength 1 | 원격 응답의 멱등 식별자 (§6.3). relay 미경유 판정(DenyAll, 강제 deny)에는 부재 |
| `operation_id` | string | 원격 요청의 operation_id (§2 공통 필드) |
| `actor_ref` | string | 결정 actor. 신원 미검증 기간은 값 규약 `unverified-local-operator` 접두/표시 (스키마 enum 아님) |
| `human_intent_id` | string | Rhizome 인간 입력 참조 (§6.1) |
| `correlation_id` | string | Rhizome correlation (§6.1) |
| `decision_source` | enum `local\|relay\|forced` | 판정 출처: 로컬 decider / 원격 relay / 강제 deny(timeout·mismatch·lease). 재조회 시 expired 판별 근거 |

- **하위 호환**: 기존 이벤트 전부 유효(필드 optional). 픽스처 무수정.
- **전방 강제**: T18 relay 경로에서는 request_id·response_id(있는 경우)·
  actor_ref·decision_source 기록을 **테스트로 의무화**한다 — 스키마는
  느슨하게, 경로는 엄격하게(기존 control_mode SCP-T16-001과 같은 방식의
  반대 방향이나, 여기서는 소급 무효화 불가가 이유).
- codegen: `make codegen` 재생성, `codegen-drift` 게이트가 정합 보증.
- replay/audit: 신규 필드는 프로젝션 의미 불변(파생 상태 계산에 미참여),
  T17-1 audit-accept와 무간섭. T18 완료 기준 테스트가 재조회 경로를 검증.

## 4. 대안 (비권고)

**(b) 신규 kind `subagent/approval_decision` 추가**: 승인 전용 이벤트로
분리 — 어휘 확장 비용(replay·audit·propgen·validate 전부 학습 필요)이
크고, 기존 policy/decision과 의미 중복이 생긴다. 스키마 어휘는 이 저장소에서
가장 되돌리기 비싼 산출물(T1)이므로 kind 추가보다 payload 확장이 싸다.

**(c) 로그 밖 relay 전용 durable 저장소**: [H] 비준 조건(예외 한정·선례화
금지) 위반 — 검토 대상 아님.

## 5. 요청 절차

1. [H]가 본 제안(3안 또는 4안 (b))을 승인/수정/기각.
2. 승인 시: contracts/events.schema.json 수정 + `make codegen` + 스키마
   유효/위반 샘플 테스트 보강 — **스키마 커밋은 [H] 리뷰 필수**(T1).
3. BLOCKED.md의 SCP-T18-001 항목 해소 후 T18 재개.

참고: 필드 의미는 Rhizome 소유 공개 계약 §6.1에서 오므로, 필드명·의미
확정 시 Rhizome 리뷰어에게 사본 통지(계약 어휘와 어긋나지 않게).
