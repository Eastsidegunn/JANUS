# SCP-T16-001 — Codex control-mode marker

## Status

Proposed; implementation blocked pending contract approval.

## Problem

Codex `exec --json` has no per-command synchronous approval hook. Its adapter
therefore uses spawn-time policy and container isolation, which must be
distinguishable from Claude's tool-level approval in durable audit events.

## Contract change required

Add an explicit, closed-enum control-mode field to the appropriate subagent
spawn/ready payload (for example `spawn_time` versus `tool_approval`). The
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

## Placement recommendation (리뷰어 분석, 2026-09-07)

저장소 대조 결과 배치를 다음으로 확정 제안한다 — 명세 소유자 판단용:

- **위치: `subagent/spawn` payload** (`ready`가 아니라). 통제 방식은 세션이
  열릴 때 정해지는 spawn 속성이지 어댑터 준비 상태가 아니다. 그리고 audit/replay가
  spawn 이벤트를 세션 통제의 기준점으로 읽으므로, 여기 있어야 "이 세션의 command
  execution은 개별 승인 대상이 아니다"를 로그 초입에서 확정할 수 있다.
- **형태: base required `control_mode` 판별 enum** `["tool_approval","spawn_time"]`.
  - `world_backend:none` 분기 → `control_mode` const `"tool_approval"`
    (기존 host procgroup/Claude 경로의 의미 보존, 값이 기존 동작과 일치)
  - `local-podman` 분기(확장 유무 2형태) → 어댑터에 따라 갈림:
    Claude는 `"tool_approval"`, Codex는 `"spawn_time"`. 두 값 모두 허용하되
    required로 두어 **누락 시 저장 거부**.
- **비호환성**: base에 required 필드를 더하므로 기존 spawn 샘플이 전부
  갱신 대상이다. T10 SCP·T11 SCP와 같은 원자적 커밋(schema·codegen·emitter·
  샘플). "additive only"라는 초안 표현은 정정한다 — base required 추가는
  기존 payload를 무효화하므로 **비호환 강화**다.
- **wire mirror**: `subagent/spawn`은 core가 쓰는 kind이지 adapter→core wire
  event가 아니므로(T10 SCP 선례) wire mirror는 만들지 않는다.

`hx audit`은 이 판별을 읽어 `spawn_time` 세션의 command execution을
`tool_approval` 세션과 구분 표기한다. 이것이 §Q5 조건 1(강등을 감사에서 구분)의
구현 접점이다.

## Evidence

Current `subagent/spawn` and `subagent/ready` payloads, plus tool and approval
payloads, have no control-mode property and are closed schemas. Adding a
marker in the adapter without this SCP would create data rejected by contracts
or misrepresent another field's semantics.
