# T9 사전 제안 — Claude Code 어댑터 계약 (승인 handshake·공용 payload 어휘)

상태: **승인 대기** (2026-08-18, 3판). 문서만 — contracts 변경도, 신규
의존성도, 어댑터 코드도 없다. 승인 후 §9 순서로 구현한다.

3판은 2차 리뷰의 차단 6건을 반영했다. 근거는 (a) T8 픽스처 15건 전수 조사,
(b) 공식 훅·헤드리스·CLI 레퍼런스, (c) 로컬 CLI 도움말이다.

---

## 1. permission_denials를 approval_request로 쓸 수 없다

T8 `05-approval-denied` 원문:

```json
{"type":"system","subtype":"permission_denied","tool_name":"Write",
 "tool_use_id":"toolu_01GR2cQ4WBoyGcN1f2MDmHoH",
 "decision_reason_type":"workingDir",
 "decision_reason":"Path is outside allowed working directories"}
```

후속 `user` 이벤트는 `tool_result{is_error:true}` +
`tool_result_meta:[{non_execution_kind:"user-rejected"}]`. 즉 **CLI가 이미
거부를 끝낸 뒤의 통보**이며 부모가 개입할 창이 없다. 이를
`subagent/approval_request`로 매핑하면 FR-POL-05를 이름만 흉내내므로 하지
않는다 — 정규화는 §3.3의 `tool_result{status:"rejected"}` 한 건뿐.

### 네이티브 어휘 (claude-code 8건 실측)

| top-level type | 건수 | subtype |
|---|---|---|
| `system` | 20 | `init` 8, `thinking_tokens` 11, `permission_denied` 1 |
| `assistant` | 18 | — (content: `text` 7, `tool_use` 8, `thinking` 3) |
| `user` | 9 | — (content: `tool_result` 8, `text` 1) |
| `rate_limit_event` | 8 | — |
| `result` | 8 | `success` 7, `error_during_execution` 1 |

최장 줄 2177 bytes(05). 도달 가능성 판단은 §3.4.

---

## 2. 승인 handshake

### 2.1 defer 조사 결론 정정

1판의 "재개 절차가 문서화되지 않음"은 **철회**한다. 레퍼런스는
`deferred_tool_use` → 외부 판정 → `claude -p --resume` → `PreToolUse` 재실행을
명시한다(내 fetch가 대용량 페이지 발췌로 해당 절을 표면화하지 못했다).
A/B 비교는 **세션 저장·격리 비용**으로만 판단한다.

### 2.2 A/B 비교

| | A. 동기 블로킹 훅 (**추천**) | B. defer + resume |
|---|---|---|
| 세션 저장 | **불필요** (`--no-session-persistence` 유지) | **필수** — resume 대상 세션이 디스크에 남아야 함 |
| 격리 비용 | 없음 | 저장된 세션이 호스트 잔존 상태가 되어 NFR-06·T10 격리에 부담 |
| 판정 지연 한도 | 훅 timeout (기본 600초, 조정 가능) | 사실상 무한 |
| 실패 모드 | timeout → deny (fail-closed) | 재개 실패 시 툴 콜 보존된 고아 세션 — 정리 책임 추가 |

**추천 A.** 600초 초과 운용이 실제로 필요해지면 B를 재제안한다.

### 2.3 상태 머신

```
[Claude] tool 호출 → PreToolUse 훅(hxapprove) 실행 (env로 소켓 경로 주입)
 └→ 어댑터: request_id 발급 → subagent/approval_request 방출 (pump는 계속 진행)
     └→ 코어 Decider 판정 (§2.4)
         └→ policy/decision durable 기록 ← 성공이 전달의 전제
             └→ approval_response{request_id, decision, reason?} → 어댑터 → 훅
                 └→ 훅 stdout: permissionDecision allow|deny → Claude 실행/차단
```

### 2.4 코어 배선 API (차단 4)

구현자가 서로 다른 API를 만들지 않도록 소유권과 시그니처를 확정한다.

**소유: `core/policy`** (계약), **구현 배선: `surfaces/hx`** (조립 지점).

```go
// core/policy — 승인 판정 계약
type ApprovalRequest struct {
    RequestID string          // 어댑터 발급 UUID
    CallID    string          // 네이티브 tool_use_id
    ToolName  string
    Args      json.RawMessage
    SpanID    string          // 요청한 서브에이전트의 child span
}
type ApprovalDecision struct {
    Allow  bool
    Reason string            // deny면 필수(비어 있으면 계약 위반)
}
type ApprovalDecider interface {
    Decide(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}
```

**의미론**

| 조건 | 동작 |
|---|---|
| `Spec.Approval == auto` (프로파일이 **명시적으로** auto) | Decider 호출 없이 allow |
| `Spec.Approval == manual`, `Decider == nil` | **deny** (사유: "승인 결정자 미배선") — 기본값이 allow가 되는 경로 없음 |
| `Decider.Decide`가 오류 | **deny** (사유에 오류 요약) |
| `Decide`가 `Allow=false, Reason=""` | 계약 위반 → **deny**로 강제하고 어댑터 오류 |
| ctx 취소 / `stop` / 훅 timeout | 대기 전건 **deny** + 소켓·도우미 정리 |

**Spec 확장** (`seams/subagent`):
```go
type Spec struct {
    …기존…
    ProfileID string               // policy.SandboxConfig.ProfileID
    Approval  policy.ApprovalMode  // policy.SandboxConfig.Approval
    Decider   policy.ApprovalDecider  // nil이면 manual에서 항상 deny
}
```
전달 경로: `policy.Evaluate` → `SandboxConfig{ProfileID, Approval}` →
`subagent.Spec` → `Spawn`. (T10에서 SandboxConfig의 나머지 필드가 같은
경로로 확장된다.)

**manual 결정자의 입력 경로**: v0.1은 자체 UI가 비목표(§1.3)이므로 T9는
인터페이스만 코어에 두고, `surfaces/hx`가 **기본 `DenyAll`**을 배선한다.
운용 판정은 프로파일(egress·fs 스코프처럼 선언적)로 표현하며, 대화형
프롬프트는 도입하지 않는다. 이 결정 자체가 §10의 승인 항목이다.

**병렬성**: pump는 `approval_request`를 방출한 뒤 **즉시 다음 줄로 진행**한다.
판정 대기는 요청별 goroutine(훅 소켓 핸들러)에서 하며, **pump는 절대 판정을
기다리지 않는다**. 어댑터 stdin 쓰기는 뮤텍스로 직렬화(한 줄=한 메시지).

**policy/decision 귀속**: `span_id` = 해당 서브에이전트의 **child span**,
`actor` = `"parent"`(판정 주체는 부모), payload =
`{decision, profile_id, reason?}` (T1 확정 형태 그대로).

**기록 실패 시 순서**: ① 훅에 **deny 전달**(훅이 매달리지 않게 먼저) →
② 대기 중인 나머지 요청도 deny → ③ 어댑터를 오류로 종료. 기록 없이 allow가
나가는 경로는 없다(불변식 1).

### 2.5 contracts 변경 제안 (차단 2 반영)

```
command.cmd enum: task | message | stop | approval_response      ← 추가

approvalResponsePayload:
  base:   properties {request_id, decision, reason}
          required [request_id, decision]        ← 판별 필드도 base required
          additionalProperties false
  oneOf:
    ├ { decision: const "allow" }                required []
    └ { decision: const "deny"  }                required [reason]   # minLength 1
```
`request_id`는 uuid pattern. schemagen 계약(분기당 판별 const 1개, 판별 필드
required, 승인 서브셋)을 충족한다.

---

## 3. 공용 payload 어휘 — 5종 폐쇄

### 3.1 매핑 표

| kind | payload | required | Claude 출처 |
|---|---|---|---|
| `ready` | `{grade, native_session_id?, model?, tools?}` | `[grade]` | `system/init` |
| `message` | `{text}` | `[text]` | `assistant.content[].text` |
| `tool_call` | `{call_id, name, args}` | `[call_id, name, args]` | `assistant.content[].tool_use{id,name,input}` |
| `tool_result` | §3.2 | base `[call_id, status]` | `user.content[].tool_result` / `system/permission_denied` |
| `approval_request` | `{request_id, call_id, name, args, reason?}` | `[request_id, call_id, name, args]` | PreToolUse 훅 입력 |

`approval_request.call_id`는 **필수**다 — PreToolUse 훅 입력에 `tool_use_id`가
있음을 문서에서 확인했다(공통 입력 `session_id`/`cwd`/`permission_mode`와 함께
이벤트 필드 `tool_name`/`tool_input`/`tool_use_id` 제공).

### 3.2 tool_result — status 판별 3분기

```
base:  properties {call_id, status, output, error, reason}
       required [call_id, status]          ← 전 분기 공통 필수
       additionalProperties false
oneOf:
  ├ status "ok"       → required [output]   (object; 스칼라·문자열은 {"value": …})
  ├ status "error"    → required [error]    (string, minLength 1)
  └ status "rejected" → required [reason]   (string, minLength 1)
```

### 3.3 permission-denied 정규화 — raw 재구성 가능성 (차단 3)

리뷰가 제시한 단순 해법을 채택한다: **`system/permission_denied`에서 rejected를
방출하고 그 줄을 raw로 붙인다.** 그러면 한 이벤트의 raw 한 줄만으로 payload
전체(reason 포함)를 재구성할 수 있다.

상태 전이:

| 입력 | 동작 |
|---|---|
| `system/permission_denied{tool_use_id, decision_reason}` | `tool_result{call_id, status:"rejected", reason:decision_reason}` **방출**, raw = 이 줄. call_id를 `rejectedEmitted`로 표시 |
| 후속 `user.tool_result{tool_use_id ∈ rejectedEmitted}` + `non_execution_kind:"user-rejected"` | **확인용 중복 통보로 소비** — 이벤트 미방출(중복 방지) |
| 후속 `user.tool_result{tool_use_id ∈ rejectedEmitted}`인데 `user-rejected`가 아님 | **프로토콜 위반(오류)** — 거부했는데 다른 결과가 온 것 |
| `rejectedEmitted` 없이 `non_execution_kind:"user-rejected"`만 도착 | 그 줄로 `rejected` 방출(reason = tool_result content), raw = 그 줄 |

"두 원본 → 한 이벤트"의 raw 표현을 새로 정의할 필요가 없어진다.

### 3.4 무시 화이트리스트 — 도달 가능한 것만 (차단 5)

우리가 실제로 쓰는 플래그에서 **도달 가능한 이벤트만** 무시한다:

| 이벤트 | 처리 | 근거 |
|---|---|---|
| `system/init` | 매핑(→`ready`) | 픽스처 실측 |
| `system/thinking_tokens` | 무시 | 픽스처 실측(11건) |
| `rate_limit_event` | 무시 | 픽스처 실측(8건) |
| `assistant.content[].thinking` | 무시 | 픽스처 실측(3건) |
| `system/api_retry` | 무시 | 네트워크 재시도는 플래그와 무관하게 발생 가능 — **합성 입력 테스트로 무시를 고정**(픽스처에 없으므로 근거 없는 무시로 두지 않는다) |
| `system/plugin_install`, `hook_started`/`hook_progress`/`hook_response` | **오류(격리 위반)** | 우리 플래그는 플러그인·자동 훅 발견을 차단한다. 나타나면 격리·플래그 계약이 깨진 것이므로 조용히 무시하면 안 된다 |
| 그 외 미지 type/subtype | **오류** | 조용한 무시 금지 |

---

## 4. raw 의미

- 원본 NDJSON 한 줄의 delimiter 제외 바이트를 표준 base64로 보존(FR-ADP-04).
- **1원본 → N정규화면 동일 raw를 각각 첨부**(assistant 한 줄이 text+tool_use를
  담는 실제 사례).
- **훅 경유 `approval_request`의 raw는 훅이 전달한 JSON 입력 바이트**를 보존
  (빈 raw 아님).
- 빈 base64 `""`는 upstream 원본이 정말 없는 합성 이벤트에만.

---

## 5. usage

### 5.1 근거 — assistant 합계 ≠ result

| 픽스처 | assistant 합 (in/cc/cr/out) | result.usage |
|---|---|---|
| 03-multi-tool | (8, 5281, 11106, **25**) | (6, 2814, 9623, **347**) |
| 05-approval-denied | (6, 7026, 3331, **9**) | (4, 3695, 3331, **642**) |
| 07-command-fail | (6, 5133, 7575, **9**) | (4, 2676, 5869, **219**) |

### 5.2 결정 (차단 2의 캐시 필드 포함)

- `subagent/usage`는 **`result`에서 한 번만** 방출.
- `input_tokens = input_tokens + cache_creation_input_tokens + cache_read_input_tokens`,
  `output_tokens = output_tokens`. 분해는 raw에.

| 상황 | 처리 |
|---|---|
| `result.usage` **객체 자체 부재** | 이벤트 **생략** + 코어 폴백 (FR-ADP-07, 정상 경로) |
| **핵심값**(`input_tokens`, `output_tokens`) 누락 | **오류** (조용한 0 대체 금지 — 비용 은폐) |
| **캐시 보조값**(`cache_creation_input_tokens`, `cache_read_input_tokens`) 누락 | **0으로 간주** (캐시 미사용 시 생략될 수 있는 선택 필드) |
| 임의 값이 음수이거나 3항 합이 int64 overflow | **오류** (checked addition) |

---

## 6. 관측 등급

`ready.grade = "observable"` (FR-ADP-06).

근거(정정): 픽스처 8건 중 **tool 이벤트가 있는 파일은 6건(02~07)**이며
tool_use·tool_result는 각각 8개다(01은 툴 없는 정상, 08은 툴 호출 전 중단).

검증(§8): 매핑 대상 중간 이벤트 **유실 0** — 네이티브 tool_use 수 = 정규화
`tool_call` 수, tool_result 수 = `tool_result` 수(§3.3의 rejected 경로 포함).

---

## 7. 격리와 자격증명 선결 조건 (차단 1)

### 7.1 문서 확인 결과

- `--settings`: "Values you set here override the same keys in your
  settings.json files… **Keys you omit keep their file-based values**"
  → **단독으로는 격리가 아니다**(사용자 훅이 살아 있다).
- `--bare`: "skip auto-discovery of hooks, skills, plugins, MCP servers,
  auto memory, and CLAUDE.md" — 격리는 되지만 **OAuth·키체인을 읽지 않고**
  `ANTHROPIC_API_KEY` 또는 `apiKeyHelper`를 요구한다.
- `--setting-sources <user|project|local>`: 설정 소스 선택. **훅 로딩을
  게이트하는지, 빈/부분 값이 허용되는지는 문서에 없다**(미확인).
- `--safe-mode`: 훅 자체를 끔 → 사용 불가.

### 7.2 격리 방안 — OAuth 유지 우선, API key는 승인 후 대안

| | C안 (**추천, 우선 시도**) | A키안 (대안) |
|---|---|---|
| 조합 | pristine 임시 작업공간 + `--setting-sources`로 사용자 소스 제외 + `--settings` 인라인 훅 | `--bare` + `--settings` 인라인 훅 |
| 인증 | **OAuth 유지** (T8과 동일 조건) | **API key 필요** — `ANTHROPIC_API_KEY` 또는 `apiKeyHelper` |
| 미확인 | `--setting-sources`의 훅 게이팅·빈 값 허용 여부 | 없음(문서상 확정) |
| 사람 선결 조건 | 없음 | **API key 준비·사용 승인 [H]** |

### 7.3 [H] 선결 조건 확정 요청

smoke를 실행하려면 **아래 중 하나가 승인**돼야 한다. T10으로 미루지 않는다.

1. **(추천) C안 우선 시도 승인** — OAuth 그대로 smoke를 돌려
   `--setting-sources` 격리와 훅 발화를 동시에 확인. 성공하면 API key 불필요.
2. **A키안 대비 API key 승인** — C안이 실패할 경우에 한해
   `ANTHROPIC_API_KEY`(또는 승인된 `apiKeyHelper`) 사용을 사전 승인.
3. 둘 다 불가하면 §2.2 B안 또는 다른 격리 방식으로 **재제안**.

1만 승인하고 2를 보류해도 되지만, 그 경우 C안 실패 시 T9는 **재제안으로
되돌아간다**(구현 완료 불가). 이 분기를 승인 시 명시해달라.

### 7.4 사람 smoke는 완료 기준에 포함

| | 내용 |
|---|---|
| 주체·시점 | 사람([H]), 구현 PR의 **최종 승인 전 1회** |
| CI | 미포함 (네트워크·인증 필요) |
| 절차 | ① 격리 확인: pristine 작업공간에서 사용자 훅이 발화하지 않음 ② 우리 훅 발화 → `approval_request` 방출 ③ **deny 응답 시 툴 미실행** ④ **allow 응답 시 실행** ⑤ 사용 커맨드·결과를 PR에 기록 |
| 실패 시 | **머지 불가** — §7.3의 2 또는 3으로 |

훅 도우미는 이 저장소가 빌드하는
`seams/subagent/claudecode/hxapprove`이며, 어댑터가 env로 넘긴 유닉스 소켓
경로로 판정을 주고받는다. **신규 의존성 없음**.

---

## 8. 스냅샷·회귀 설계 (네트워크·실 인증 없음)

### 8.1 3층 분리

1. **Claude 8건 골든** — 입력 NDJSON → 정규화 이벤트 전체 비교. 각 골든에
   무시된 네이티브 이벤트 목록 기록.
2. **T8 전체 15건 fingerprint** — `make fixtures`에서 매니페스트 재실행 +
   파일별 줄 수·SHA-256·type 히스토그램 대조(Codex 7건 포함).
3. **Codex 7건은 정규화하지 않는다** — T14 범위.

### 8.2 크기 계약

| 입력 | 기대 |
|---|---|
| **64KiB 초과 유효 줄** | **정상 처리** (Scanner 기본 한계 회귀 방지) |
| **4MiB**(기존 pump와 동일) 초과 | fail-closed 오류 |

### 8.3 result → done 매핑

픽스처 실측: `success` 7건 전부 `terminal_reason="completed"`; 중단 1건은
`error_during_execution` + `aborted_streaming` + `result` 문자열 **없음**.

```
status = ok      : subtype == "success"
         stopped : 어댑터가 stop 명령을 보낸 뒤의 종료(1순위)
                   또는 terminal_reason ∈ {aborted_streaming, aborted}
         error   : 그 외 비-success
result = 있으면 그대로 / 없으면 결정적 문구:
         "(결과 없음: subtype=<subtype>, terminal_reason=<terminal_reason|none>)"
```

### 8.4 fail-closed 회귀 목록

| 케이스 | 기대 |
|---|---|
| 64KiB 초과 유효 줄 | **정상 처리** |
| 4MiB 초과 줄 | 오류 |
| 빈 줄 / 잘못된 JSON | 오류 |
| 화이트리스트 밖 미지 type/subtype | 오류 |
| `plugin_install`·`hook_*` 출현 | **오류(격리 위반)** |
| `api_retry` 합성 입력 | 무시(정상) |
| `result.usage` 객체 부재 | usage 생략, 나머지 정상 |
| usage 핵심값 누락·음수·overflow | 오류 |
| 캐시 보조값 누락 | 0으로 간주(정상) |
| `rejectedEmitted` 뒤 non-user-rejected 결과 | 오류 |
| 미상관·중복 request_id의 approval_response | 오류 |
| deny인데 reason 없음 | 오류 |
| 판정 timeout / stop / ctx 취소 | 대기 전건 deny + 정리 |
| `policy/decision` 기록 실패 | deny 전달 → 나머지 deny → 어댑터 오류 종료 |
| `Decider == nil` + manual | deny |

---

## 9. 구현 순서 (승인 후)

1. 순수 parser + Claude 8건 골든
2. **명세 변경(SCP-002 승인)** → contracts 반영(§2.5 command, §3 payload 5종)
   → codegen 재생성
3. 독립 실행 어댑터 (`seams/subagent/claudecode`)
4. 승인 handshake — `hxapprove` + 소켓 프로토콜 + §2.4 코어 배선, fake 테스트
5. `make fixtures` 활성화(§8.1의 2층)
6. **사람 smoke(§7.4)** → traceability → 최종 [H] 승인

---

## 10. 승인 요청 항목

| # | 항목 | 성격 |
|---|---|---|
| 1 | **SCP-002 — 명세 변경**(§5.2 표에 `approval_response` 추가 + FR-ADP-02 최소 계약에 approval 추가) | **명세 [H]** |
| 2 | §2.5 `approval_response` 스키마(base required 포함) | **contracts [H]** |
| 3 | §3 payload 5종 폐쇄(call_id 필수, tool_result base required) | **contracts [H]** |
| 4 | **§7.3 smoke 자격증명 선결 조건** — C안 우선 시도 / API key 사전 승인 여부 | **[H] 선결** |
| 5 | §2.4 코어 배선 API(ApprovalDecider, nil=deny, DenyAll 기본, 병렬·귀속·순서) | 설계 결정 |
| 6 | §3.3 permission-denied를 system 줄에서 방출(raw 재구성 가능) | 설계 결정 |
| 7 | §3.4 화이트리스트 축소 + 격리 위반 이벤트를 오류로 | 설계 결정 |
| 8 | §5.2 usage 핵심값/캐시 보조값 구분 | 설계 결정 |
| 9 | §8 스냅샷·크기·매핑·회귀 목록 | 테스트 설계 |
