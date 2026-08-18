# T9 사전 제안 — Claude Code 어댑터 계약 (승인 handshake·공용 payload 어휘)

상태: **승인 대기** (2026-08-18, 2판). 이 PR은 문서만 담는다 — contracts 변경도,
신규 의존성도, 어댑터 코드도 없다. 승인 후 §9 순서로 구현한다.

2판은 1차 리뷰의 차단 8건을 반영했다. 근거는 (a) T8 픽스처 15건 전수 조사,
(b) 공식 훅·헤드리스 문서, (c) 로컬 CLI 도움말에서 왔다.

---

## 1. 조사 결과 — permission_denials를 approval_request로 쓸 수 없다

T8 `05-approval-denied`의 스트리밍 이벤트 원문:

```json
{"type":"system","subtype":"permission_denied","tool_name":"Write",
 "tool_use_id":"toolu_01GR2cQ4WBoyGcN1f2MDmHoH",
 "decision_reason_type":"workingDir",
 "decision_reason":"Path is outside allowed working directories"}
```

후속 `user` 이벤트는 `tool_result{is_error:true}` +
`tool_result_meta:[{non_execution_kind:"user-rejected"}]`를 담는다.
즉 **CLI가 이미 거부를 끝낸 뒤의 통보**이며 부모가 개입할 창이 없다.
이를 `subagent/approval_request`로 매핑하면 FR-POL-05를 이름만 흉내내는
것이므로 하지 않는다. 정규화는 §3.2의 `tool_result{status:"rejected"}` 한 건뿐.

### 네이티브 어휘 (claude-code 8건 실측 + 문서 보강)

| top-level type | 픽스처 건수 | subtype |
|---|---|---|
| `system` | 20 | `init` 8, `thinking_tokens` 11, `permission_denied` 1 |
| `assistant` | 18 | — (content: `text` 7, `tool_use` 8, `thinking` 3) |
| `user` | 9 | — (content: `tool_result` 8, `text` 1) |
| `rate_limit_event` | 8 | — |
| `result` | 8 | `success` 7, `error_during_execution` 1 |

문서에서 확인한 **픽스처에 없지만 실재하는** 이벤트(무시 화이트리스트에 포함):
`system/api_retry`, `system/plugin_install`, `hook_started`/`hook_progress`/
`hook_response`, `stream_event`(`--include-partial-messages` 사용 시 — T9는
이 플래그를 쓰지 않는다). 최장 줄 2177 bytes(05).

---

## 2. 승인 handshake

### 2.1 defer 조사 결론 정정 (차단 1)

1판의 "재개 절차가 문서화되지 않음"은 **철회한다.** 리뷰 인용대로 공식 훅
레퍼런스는 `deferred_tool_use` 수신 → 외부 판정 → `claude -p --resume` →
`PreToolUse` 재실행 절차를 명시한다. (내 두 차례 fetch에서는 대용량 페이지의
발췌 문제로 해당 절이 표면화되지 않았다. 리뷰 인용을 근거로 정정하며,
구현 시 원문을 다시 확인한다.) 따라서 A/B 비교는 **문서화 여부가 아니라
세션 저장·격리 비용**으로만 판단한다.

### 2.2 A/B 비교 — 세션 저장·격리 비용

| | A. 동기 블로킹 훅 (**추천**) | B. defer + resume |
|---|---|---|
| 세션 수명 | 하나로 유지 | 판정마다 프로세스 종료·재개 |
| 세션 저장 | **불필요** — `--no-session-persistence` 유지 가능 | **필수** — 세션 파일이 디스크에 남아야 resume 가능 |
| 격리 비용 | 없음 | 세션 저장소 경로·수명·정리를 어댑터가 소유해야 하고, 저장된 세션은 T10 샌드박스 경계 밖 호스트 상태가 된다 |
| 판정 지연 한도 | 훅 timeout(기본 600초, `timeout` 필드로 조정) | 사실상 무한 |
| 실패 모드 | timeout → deny (fail-closed, §2.4) | 재개 실패 시 툴 콜이 보존된 채 세션이 고아가 됨 — 정리 책임 추가 |
| 상태 복잡도 | 요청/응답 1왕복 | 종료·재개·재실행의 3단계 + session id 수명 관리 |

**추천 A.** B는 판정 지연 한도가 없는 대신 세션 저장을 강제하고, 저장된
세션이 호스트에 남는 것은 NFR-06(신뢰 경계는 컨테이너 벽)과 T10의 격리
설계에 부담을 준다. 판정이 600초를 넘는 운용이 실제로 필요해지면 그때
B를 재제안한다(§10의 보류 항목).

### 2.3 상태 머신

```
[Claude] tool 호출 시도
 └→ PreToolUse 훅(hxapprove) 실행 — 어댑터가 env로 소켓 경로 주입
     └→ 어댑터: request_id 발급 → subagent/approval_request 방출
         └→ 코어(승인 결정자): 프로파일 기반 판정
             └→ policy/decision 이벤트 durable 기록 ← 기록 성공이 전달의 전제
                 └→ 코어→어댑터: approval_response{request_id, decision, reason?}
                     └→ 어댑터→훅: 판정 전달
                         └→ 훅 stdout: permissionDecision allow|deny
                             └→ Claude: 실행 또는 차단(사유가 모델에 전달)
```

### 2.4 코어 측 계약 (차단 3)

**결정자와 기본값**
- 승인 결정자는 코어의 정책 레이어다. T9는 `subagent/approval_request`의
  **소비자**를 명시적으로 배선한다 — 로그에 쓰고 끝나는 경로를 만들지 않는다.
- 프로파일 승인 모드가 `manual`이면 결정자는 호출자(부모 세션의 운용 주체)다.
  **결정자가 없거나 응답하지 않으면 기본 deny.**
- `auto` allow는 **프로파일이 명시적으로 `auto`일 때만** 허용한다
  (T6 `ApprovalMode`가 zero value로 auto가 될 수 없게 파서에서 이미 강제).
- 병합 규칙상 auto는 양쪽이 auto일 때만 유지되므로(FR-POL-03), 오버레이가
  승인 게이트를 푸는 경로는 없다.

**기록이 전달보다 먼저**
- 판정은 `policy/decision` 이벤트로 **durable 기록된 뒤에만** 훅에 전달한다.
- **기록 실패 시 deny.** allow를 기록 없이 전달하면 "로그 밖의 진실"이 생긴다
  (불변식 1). writer가 terminal 상태면 그 자체로 deny다.

**동시성·정리**
- 툴 콜은 병렬로 뜰 수 있다. request_id별 독립 대기이며, **어댑터 stdin의
  command 쓰기는 직렬화**한다(부분 줄 섞임 방지 — 한 줄 = 한 메시지, §5.2).
- 미상관·중복·미지 request_id의 `approval_response`는 프로토콜 위반(오류).
- **context 취소·`stop` 명령·timeout 시: 대기 중인 모든 요청을 deny로 마감하고
  소켓·훅 도우미 프로세스를 정리한다.** 대기 요청을 남긴 채 종료하지 않는다.

**deny의 사유 필수**
- `approval_response`는 decision 판별 분기로 만든다:
  `allow`는 reason 선택, **`deny`는 비어 있지 않은 reason 필수.**
  (T5 `hookVerdictPayload`의 reject 분기와 같은 의미론.)

### 2.5 contracts 변경 제안 (승인 대상)

```
command.cmd enum: task | message | stop | approval_response   ← 추가

approvalResponsePayload:  # decision 판별 oneOf
  ├ { decision: "allow", request_id: uuid, reason?: string }   required [request_id]
  └ { decision: "deny",  request_id: uuid, reason: string(minLength 1) }
                                                               required [request_id, reason]
  additionalProperties false
```

schemagen 계약(분기당 판별 const 1개, 승인 서브셋) 안에서 표현된다.

---

## 3. 공용 payload 어휘 — 5종 폐쇄 (T14 Codex와 공유)

### 3.1 매핑 표

| kind | payload (제안) | Claude 출처 | Codex 출처(T14 검증용) |
|---|---|---|---|
| `ready` | `{grade:"observable"\|"opaque", native_session_id?, model?, tools?:[string]}` | `system/init` | `thread.started` |
| `message` | `{text: string}` | `assistant.content[].text` | `item.completed{agent_message}.text` |
| `tool_call` | `{call_id, name, args: object}` | `assistant.content[].tool_use{id,name,input}` | `item.started{command_execution}` |
| `tool_result` | §3.2 (status 판별 3분기) | `user.content[].tool_result` | `item.completed{command_execution}` |
| `approval_request` | `{request_id, call_id?, name, args: object, reason?}` | PreToolUse 훅 입력 | (T14: 해당 없음) |

`call_id`는 도구 네이티브 ID를 불투명 문자열로 보존한다(Claude `toolu_…`,
Codex `item_N`). 도구 고유 필드는 전부 `raw`로 간다.

### 3.2 tool_result — status 판별 3분기 (차단 6)

T5 `toolResultPayload`와 같은 어휘·같은 필수 규칙을 쓴다:

| status | 필수 | 의미 |
|---|---|---|
| `ok` | `output` (object) | 정상 결과. 스칼라·문자열 출력은 T5와 동일하게 `{"value": …}`로 감싼다 |
| `error` | `error` (minLength 1) | 툴 실행 오류 (`tool_result.is_error=true`) |
| `rejected` | `reason` (minLength 1) | 승인 거부로 실행되지 않음 |

### 3.3 중복 방지 상태 전이 (차단 6)

거부 1건이 `system/permission_denied`와 후속 `user.tool_result`(+
`tool_result_meta.non_execution_kind`) **두 줄**로 나타난다. 같은 call_id의
결과가 두 번 나오면 안 되므로:

- `system/permission_denied`는 **정규화 이벤트를 만들지 않는다.** 대신
  call_id → `pendingRejection{reason}` 상태만 기록한다.
- 후속 `user.tool_result`가 오면 그 한 건만 방출하되, 해당 call_id가
  `pendingRejection`이면 `status:"rejected"`(reason=기록된 decision_reason),
  아니면 `is_error`에 따라 `error`/`ok`로 방출한다.
- `pendingRejection`인데 후속 `tool_result`가 오지 않고 `result`에 도달하면
  **프로토콜 위반(오류)** — 조용히 삼키지 않는다.
- `tool_result_meta.non_execution_kind`가 `user-rejected`인데
  `pendingRejection`이 없으면 그것만으로 `rejected`로 방출한다(단독 근거).

### 3.4 무시 화이트리스트

`system/thinking_tokens`, `system/api_retry`, `system/plugin_install`,
`hook_started`/`hook_progress`/`hook_response`, `rate_limit_event`,
`assistant.content[].thinking`. **화이트리스트 밖의 미지 type/subtype은
오류**(조용한 무시 금지, §8). 무시 목록은 각 골든에 명시 기록해 의도적 무시와
누락을 구분한다.

---

## 4. raw 의미 (차단 5)

- **원본 NDJSON 한 줄의 delimiter(개행) 제외 바이트를 표준 base64로 보존**
  (FR-ADP-04). 재인코딩·재정렬·필드 삭제 금지.
- **1원본 → N정규화면 동일 raw를 각각 첨부**한다(assistant 한 줄이
  text+tool_use를 담는 실제 사례 근거).
- **훅 경유 `approval_request`의 raw는 `""`가 아니다** — PreToolUse 훅이
  전달한 JSON 입력이 실제 원본이므로 **그 바이트를 base64로 보존**한다.
- 빈 base64 `""`는 upstream 원본이 정말 없는 합성 이벤트에만 쓴다.

---

## 5. usage (차단 7)

### 5.1 근거 — assistant 합계 ≠ result

| 픽스처 | assistant usage 합 (in/cc/cr/out) | result.usage |
|---|---|---|
| 03-multi-tool | (8, 5281, 11106, **25**) | (6, 2814, 9623, **347**) |
| 05-approval-denied | (6, 7026, 3331, **9**) | (4, 3695, 3331, **642**) |
| 07-command-fail | (6, 5133, 7575, **9**) | (4, 2676, 5869, **219**) |

assistant 스트리밍 usage는 부분값이고, 멀티턴에서는 입력도 과대 계상된다.

### 5.2 결정

- `subagent/usage`는 **`result` 이벤트에서 한 번만** 방출한다.
- `input_tokens = input_tokens + cache_creation_input_tokens + cache_read_input_tokens`,
  `output_tokens = output_tokens`. 분해는 raw에 남는다.
- **누락과 손상을 구분한다** (FR-ADP-07은 usage 자체를 SHOULD로 두고 미보고 시
  코어 폴백을 요구한다):

| 상황 | 처리 |
|---|---|
| `result.usage` **객체 자체가 없음** | `subagent/usage`를 **생략** — 코어의 시간·요금 기반 폴백에 맡긴다 (정상 경로, 오류 아님) |
| usage 객체는 있는데 핵심값(`input_tokens`/`output_tokens`) 누락 | **오류** (조용한 0 대체 금지 — 비용 은폐) |
| 값이 음수이거나 3항 합산이 int64 overflow | **오류** (checked addition, T4 `logd` 정책과 동일 방향) |

---

## 6. 관측 등급

`ready.grade = "observable"` (FR-ADP-06). 근거: tool_use/tool_result가
스트림에 실재(8건 중 7건). 검증(§8): 매핑 대상 중간 이벤트 **유실 0** —
네이티브 tool_use 수 = 정규화 `tool_call` 수, tool_result 수 = `tool_result` 수.

---

## 7. 훅 주입 격리 + 사람 smoke (차단 2)

### 7.1 격리 방식 — `--bare` 기반

헤드리스 문서 확인 결과가 1판의 `--setting-sources` 추정보다 정확하다:

- **`--bare`**: "skipping auto-discovery of hooks, skills, plugins, MCP
  servers, auto memory, and CLAUDE.md" — 사용자·프로젝트 설정 상속을 끊는다.
  문서의 로드 표에 "Settings → `--settings <file-or-json>`"가 명시되어,
  **bare 상태에서 우리 훅만 인라인으로 주입**하는 조합이 문서화된 경로다.
- `--safe-mode`는 훅 자체를 끄므로 쓸 수 없다(로컬 도움말 확인). T8 픽스처가
  이 플래그로 녹화됐기 때문에 픽스처에는 훅 경유 승인 이벤트가 없다 — §8의
  스냅샷 설계가 이를 전제한다.
- 주의: bare 모드는 OAuth·키체인을 읽지 않고 `ANTHROPIC_API_KEY`(또는
  `--settings`의 `apiKeyHelper`)를 요구한다 — 자격증명 주입 방식이 T10의
  FR-SBX-04(단기 토큰 주입)와 맞물리므로 그때 재확인한다.

훅 프로그램은 이 저장소가 빌드하는 도우미
(`seams/subagent/claudecode/hxapprove`)이며, 어댑터가 env로 넘긴 유닉스 소켓
경로로 판정을 주고받는다. **신규 의존성 없음** — SDK·MCP·외부 라이브러리를
도입하지 않는다.

### 7.2 사람 smoke는 T9 완료 기준에 **포함**한다

리뷰 지적을 수용한다. `--bare` + 인라인 `--settings`에서 훅이 실제로 발화하고
allow/deny가 실행을 게이트하는지는 **A안의 성립 조건**이므로, 확인 없이
"구현 완료"로 머지할 수 없다.

| | 내용 |
|---|---|
| 실행 주체 | 사람([H], 실 자격증명) |
| 시점 | 구현 PR의 **최종 [H] 승인 전 1회** |
| CI | 넣지 않는다(네트워크·인증 필요) |
| 절차 | ① 임시 작업공간에서 `claude --bare -p … --settings '<T9 훅 JSON>'` 실행 ② 훅이 호출되어 `approval_request`가 방출되는지 ③ deny 응답 시 툴이 실행되지 않는지 ④ allow 응답 시 실행되는지 — 결과와 커맨드를 PR에 기록 |
| 실패 시 | T9는 머지 불가. §2.2 B안 또는 대체 격리 방식으로 **재제안** |

---

## 8. 스냅샷·회귀 설계 (네트워크·실 인증 없음)

### 8.1 3층 분리

1. **Claude 8건 골든 대조** — 입력 NDJSON → 정규화 이벤트 전체를 골든과 비교.
   각 골든에 무시된 네이티브 이벤트 목록을 함께 기록.
2. **T8 전체 15건 fingerprint** — `make fixtures`에서 매니페스트 재실행 +
   파일별 원본 fingerprint(줄 수, SHA-256, top-level type 히스토그램) 대조.
   Codex 7건도 이 층에는 포함(포맷 변경 검출, FR-ADP-05).
3. **Codex 7건은 정규화하지 않는다** — T14 범위.

### 8.2 크기 계약 정정 (차단 8)

64KiB는 `bufio.Scanner`의 기본 한계일 뿐 계약이 아니다. 유효한 대형 tool
result는 흔하므로 **크기를 이유로 거부하지 않는다.**

| 입력 | 기대 |
|---|---|
| **64KiB 초과 유효 줄** | **정상 처리** (Scanner 기본 한계 회귀 방지 — 명시적 회귀 테스트) |
| 명시 상한(**4MiB**, 기존 pump와 동일) 초과 | fail-closed 오류 |

### 8.3 result → done 매핑 (차단 8)

픽스처 실측: `success` 7건은 전부 `terminal_reason="completed"`,
중단 1건은 `subtype=error_during_execution`, `terminal_reason="aborted_streaming"`,
`result` 문자열 **없음**.

```
status =
  ok       : subtype == "success"
  stopped  : 어댑터가 stop 명령을 보낸 뒤의 종료  ← 1순위(어댑터 자신의 상태)
             또는 terminal_reason ∈ {aborted_streaming, aborted}
  error    : 그 외 모든 비-success
result 문자열 =
  있으면 그대로. 없으면 결정적 문구:
    "(결과 없음: subtype=<subtype>, terminal_reason=<terminal_reason|none>)"
```
결정적 문구는 골든에 그대로 박히므로 임의 시각·난수를 넣지 않는다.

### 8.4 fail-closed 회귀 목록

| 케이스 | 기대 |
|---|---|
| 64KiB 초과 유효 줄 | **정상 처리** |
| 4MiB 초과 줄 | 오류 |
| 빈 줄 | 오류 (§5.2 위반) |
| 잘못된 JSON | 오류 |
| 화이트리스트 밖 미지 type/subtype | 오류 |
| `result.usage` 객체 부재 | usage 이벤트 생략, 나머지 정상 |
| usage 핵심값 누락·음수·overflow | 오류 |
| `pendingRejection` 미해소 상태로 result 도달 | 오류 |
| 미상관·중복 request_id의 approval_response | 오류 |
| 판정 timeout / stop / ctx 취소 | 대기 전건 deny + 정리 |
| policy/decision 기록 실패 | deny |

---

## 9. 구현 순서 (승인 후)

1. 순수 parser + Claude 8건 골든
2. contracts 반영 — §2.5 command 분기 + §3 payload 5종 폐쇄, codegen 재생성
3. 독립 실행 어댑터 (`seams/subagent/claudecode`)
4. 승인 handshake — `hxapprove` + 소켓 프로토콜 + 코어 측 결정자 배선(§2.4),
   fake 기반 테스트
5. `make fixtures` 활성화(§8.1의 2층)
6. **사람 smoke(§7.2)** → traceability 갱신 → 최종 [H] 승인

---

## 10. 승인 요청 항목

| # | 항목 | 성격 |
|---|---|---|
| 1 | §2.5 `approval_response` command 추가 (decision 판별, deny reason 필수) | **contracts 변경 [H]** |
| 2 | §3 payload 5종 폐쇄 (tool_result 3분기 포함) | **contracts 변경 [H]** |
| 3 | **SCP-002** — §5.2 명세와의 관계 해석 또는 명세 변경 (차단 4, 별도 문서) | **명세 [H]** |
| 4 | §2.2 A안 채택(세션 저장·격리 비용 근거), B안은 보류 | 설계 결정 |
| 5 | §2.4 코어 측 승인 계약 (결정자·기본 deny·기록 선행·직렬화·정리) | 설계 결정 |
| 6 | §5 usage 누락 생략 vs 손상 오류 구분 | 설계 결정 |
| 7 | §7 `--bare` 기반 격리, 신규 의존성 없음 | 설계 결정 |
| 8 | §7.2 사람 smoke를 **완료 기준에 포함** | 범위 결정 |
| 9 | §8 스냅샷 3층·크기 계약 정정·result→done 매핑·회귀 목록 | 테스트 설계 |
