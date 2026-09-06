# SCP-T16-001 — Codex control-mode marker

## Status

**형태 승인됨 [H] (2026-09-07)** — `subagent/spawn` base required에
`control_mode` 판별 enum을 추가하는 스키마 형태는 명세 소유자가 승인했다
(T10 world_backend·T11 egress decision 판별 패턴의 세 번째 적용).

**값은 구현자 실측 대기** — enum 값(`container_only` vs `spawn_time` 등)은
codex 실측으로 확정한다. 값이 바뀌면 enum이 바뀌므로 확정 시 [H] 재확인.
아래 §값 선택의 의도 참조.

## Problem

Codex `exec --json` has no per-command synchronous approval hook. Its adapter
therefore uses spawn-time policy and container isolation, which must be
distinguishable from Claude's tool-level approval in durable audit events.

## Contract change required

Add an explicit, closed-enum control-mode field to the appropriate subagent
spawn/ready payload (for example `tool_approval` versus `container_only`). The
field must be required for Codex sessions, forbidden/unchanged for the
`world_backend:none` and Claude paths unless their existing semantics provide
the value, and included in generated types and wire validation. Do not overload
`policy/decision`, `tools`, or `approval_request`: those fields have different
meanings and are closed by `additionalProperties:false`.

## Safety requirements

- `ApprovalManual` with no native approval remains fail-closed before any
  command effect, yielding `ErrApprovalUnavailable` and terminal `done{error}`.
- Codex policy denials are not fabricated as approval requests.
- Replay and audit must expose the marker so Codex spawn-time control cannot be
  mistaken for parent per-tool approval.
- Existing payloads remain valid; the change is additive only where the
  discriminator requires it and must preserve the none/Claude branches.

## 값 선택의 의도 — `container_only` (리뷰어, 이견 환영)

초안은 `spawn_time`이었으나 `container_only`로 바꿔 제안한다. **의도를
드러내니 구현자·[H]가 실측으로 반박할 수 있게 한다.**

논거:
- `tool_approval`(Claude)은 툴별 부모 게이트가 **작동함**이 T9 smoke로 증명됐다.
- Codex에서 **검증된 통제는 컨테이너 격리(overlay·egress)뿐**이다. 05 meta가
  확인한 것은 "승인 UI 부재"(= 훅 없음)까지이며, codex의 `-a`/sandbox 플래그가
  실제로 실행을 **좁히는지는 아직 실측되지 않았다.** 05 meta의
  "`/tmp`는 쓰기 허용이라 요청이 실행됨"은 오히려 정책이 있어도 뚫린 사례다.
- 따라서 `spawn_time`이라는 값은 "spawn-time 정책이 작동함"을 **함의**하는데,
  그 함의가 미검증이다. 표식이 검증된 것보다 강한 통제를 주장하면 안 된다
  (T12 `effect_observation` 정신 — 약함을 정확한 크기로 인정).
- `container_only`는 **가장 약한, 확실히 작동하는 통제만** 주장한다.

**구현자·[H]에게 열어두는 판단:**
- [H] smoke에서 codex spawn-time 정책이 실제로 실행을 좁히는 것이 확인되면,
  값을 세분(예: `spawn_time_policy` 추가)하는 후속 SCP를 낼 수 있다. 지금은
  검증된 최소로 고정한다.
- 구현자가 05 meta 외에 spawn-time 정책 작동의 근거를 이미 안다면, 그 근거를
  제시하고 값 이름을 재론해도 좋다. **이 값은 확정이 아니라 실측 가능한
  제안이다.**

## Placement recommendation (리뷰어 분석, 2026-09-07)

저장소 대조 결과 배치를 다음으로 확정 제안한다 — 명세 소유자 판단용:

- **위치: `subagent/spawn` payload** (`ready`가 아니라). 통제 방식은 세션이
  열릴 때 정해지는 spawn 속성이지 어댑터 준비 상태가 아니다. 그리고 audit/replay가
  spawn 이벤트를 세션 통제의 기준점으로 읽으므로, 여기 있어야 "이 세션의 command
  execution은 개별 승인 대상이 아니다"를 로그 초입에서 확정할 수 있다.
- **형태: base required `control_mode` 판별 enum** `["tool_approval","container_only"]`.
  - `world_backend:none` 분기 → `control_mode` const `"tool_approval"`
    (기존 host procgroup/Claude 경로의 의미 보존, 값이 기존 동작과 일치)
  - `local-podman` 분기(확장 유무 2형태) → 어댑터에 따라 갈림:
    Claude는 `"tool_approval"`, Codex는 `"container_only"`. 두 값 모두 허용하되
    required로 두어 **누락 시 저장 거부**.
- **비호환성**: base에 required 필드를 더하므로 기존 spawn 샘플이 전부
  갱신 대상이다. T10 SCP·T11 SCP와 같은 원자적 커밋(schema·codegen·emitter·
  샘플). "additive only"라는 초안 표현은 정정한다 — base required 추가는
  기존 payload를 무효화하므로 **비호환 강화**다.
- **wire mirror**: `subagent/spawn`은 core가 쓰는 kind이지 adapter→core wire
  event가 아니므로(T10 SCP 선례) wire mirror는 만들지 않는다.

`hx audit`은 이 판별을 읽어 `container_only` 세션의 command execution을
`tool_approval` 세션과 구분 표기한다. 이것이 §Q5 조건 1(강등을 감사에서 구분)의
구현 접점이다.

## Evidence

Current `subagent/spawn` and `subagent/ready` payloads, plus tool and approval
payloads, have no control-mode property and are closed schemas. Adding a
marker in the adapter without this SCP would create data rejected by contracts
or misrepresent another field's semantics.
