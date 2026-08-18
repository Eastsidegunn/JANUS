# T9 사전 제안 — Claude Code 어댑터 계약 (승인 handshake·공용 payload 어휘)

상태: **승인 대기** (2026-08-18). 이 PR은 문서만 담는다 — contracts 변경도,
신규 의존성도, 어댑터 코드도 없다. 승인 후 §9 순서로 구현한다.

근거는 (a) T8 픽스처 15건 전수 조사, (b) 공식 훅 문서, (c) 로컬 CLI 도움말
확인에서 왔다. 조사 커맨드와 결과는 각 절에 그대로 적었다.

---

## 1. 조사 결과 — permission_denials를 approval_request로 쓸 수 없다

리뷰 지적대로다. T8 `05-approval-denied`의 스트리밍 이벤트 원문:

```json
{"type":"system","subtype":"permission_denied","tool_name":"Write",
 "tool_use_id":"toolu_01GR2cQ4WBoyGcN1f2MDmHoH",
 "decision_reason_type":"workingDir",
 "decision_reason":"Path is outside allowed working directories",
 "message":"Claude requested permissions to write to …, but you haven't granted it yet."}
```

이어지는 `user` 이벤트는 `tool_result{is_error:true}`와
`tool_result_meta:[{non_execution_kind:"user-rejected"}]`를 담는다.

즉 **CLI가 이미 거부를 끝낸 뒤의 통보**다. 판정을 요청하지 않았고, 부모가
개입할 창이 없다. 이것을 `subagent/approval_request`로 매핑하면
"부모 정책 레이어가 판정한다"(FR-POL-05)는 계약을 이름만 흉내내는 것이므로
**하지 않는다**. 이 이벤트는 §3의 `tool_result{status:"rejected"}` +
사후 통보로 정규화한다(승인 요청 아님).

**따라서 T9의 승인 handshake는 별도 메커니즘이 필요하다 — §2.**

### 픽스처 네이티브 어휘 전수 (claude-code 8건)

| top-level type | 건수 | subtype |
|---|---|---|
| `system` | 20 | `init` 8, `thinking_tokens` 11, `permission_denied` 1 |
| `assistant` | 18 | — (content: `text` 7, `tool_use` 8, `thinking` 3) |
| `user` | 9 | — (content: `tool_result` 8, `text` 1) |
| `rate_limit_event` | 8 | — |
| `result` | 8 | `success` 7, `error_during_execution` 1 |

최장 줄 2177 bytes(05). `thinking` 블록과 `rate_limit_event`가 실재하므로
정규화 매핑에서 명시적으로 다뤄야 한다(§3 표의 '무시' 항목).

---

## 2. 승인 handshake — 코어→어댑터 응답 command와 상태 머신

### 2.1 문서 확인 결과

- `PreToolUse` 훅은 `-p`에서 동작하며 "fires before any permission-mode
  check, in every permission mode"다. `permissionDecision`은
  `allow` / `deny` / (무응답), **그리고 `-p` 한정으로 `defer`**가 있다.
  `defer`는 "exits the process with the tool call preserved so an Agent SDK
  wrapper can collect input and resume".
- `PermissionRequest` 훅의 `ask`는 대화형 프롬프트나 Agent SDK의
  `canUseTool` 콜백이 있을 때만 의미가 있다 — "In plain `-p` runs …, use
  `PreToolUse` hooks for automated permission decisions instead."
- command 훅 기본 timeout은 대부분 이벤트에서 **600초**(per-hook `timeout`
  필드로 조정 가능).

### 2.2 두 후보와 추천

| | A. 동기 블로킹 훅 (**추천**) | B. defer + resume |
|---|---|---|
| 방식 | PreToolUse command 훅이 어댑터에 요청을 보내고 **판정이 올 때까지 블록**한 뒤 allow/deny를 반환 | 훅이 `defer` 반환 → 프로세스 종료(툴 콜 보존) → 판정 후 세션 resume |
| 세션 | 하나로 유지, 재개 불필요 | 종료·재개 필요 (`--resume`/`--session-id`) |
| 시간 한도 | 훅 timeout(기본 600초, 조정 가능) | 사실상 무한 |
| 문서화 수준 | allow/deny 계약이 명시적 | **defer의 보존·재개 절차가 문서화되지 않음**(공식 문서 확인) |
| 세션 지속성 | `--no-session-persistence` 유지 가능 | 세션 저장 필요 — 격리 정책과 충돌 |

**추천 A.** B의 재개 절차가 공식 문서에 없어 계약을 정확히 고정할 수 없고,
세션 저장을 요구해 격리(§7)와 충돌한다. A는 판정이 실제로 실행을 게이트하므로
FR-POL-05를 이름이 아니라 동작으로 충족한다. 판정 지연이 훅 timeout을 넘으면
**fail-closed(거부)**로 끝난다.

### 2.3 상태 머신

```
[Claude] tool 호출 시도
   └→ PreToolUse 훅 실행 (hxapprove, 어댑터가 env로 소켓 경로 주입)
        └→ 어댑터: subagent/approval_request 방출 (request_id 발급)
             └→ 코어: FR-POL-05 판정 (프로파일의 승인 모드/정책)
                  └→ 코어→어댑터: approval_response{request_id, decision}
                       └→ 어댑터→훅: 판정 전달
                            └→ 훅 stdout: permissionDecision allow|deny
                                 └→ Claude: 실행 또는 차단(사유는 모델에 전달)
```

- `request_id`는 어댑터가 발급하는 UUID. `approval_request`와
  `approval_response`의 상관 키이며, 어댑터는 미상관·중복·미지 request_id의
  응답을 **거부(프로토콜 위반)**한다.
- 타임아웃·응답 없음·어댑터 종료는 전부 **deny**로 귀결(fail-closed).
- 한 번에 여러 툴 콜이 병렬로 뜰 수 있으므로 request_id별 독립 대기.

### 2.4 contracts 변경 제안 (승인 대상)

`wire.schema.json`의 코어→어댑터 command에 4번째 분기를 추가한다:

```
command.cmd enum: task | message | stop | approval_response   ← 추가
approvalResponsePayload:
  { request_id: string(uuid pattern),
    decision: "allow" | "deny",
    reason?: string }
  required [request_id, decision], additionalProperties false
```

schemagen 계약(분기당 판별 const 1개, 승인 서브셋) 안에서 표현된다.

---

## 3. 공용 payload 어휘 — 5종 폐쇄 (T14 Codex와 공유)

현재 `subagent/ready|message|tool_call|tool_result|approval_request`는 열린
객체다. Claude(8건)와 Codex(7건) 픽스처 양쪽에서 실제로 표현 가능한 최소
공통 형태를 확정 제안한다. **T14가 같은 어휘로 매핑 가능해야 하므로**
도구별 고유 필드는 전부 `raw`로 보낸다.

| kind | payload (제안) | Claude 출처 | Codex 출처(T14 검증용) |
|---|---|---|---|
| `ready` | `{grade: "observable"\|"opaque", native_session_id?: string, model?: string, tools?: [string]}` | `system/init`의 session_id·model·tools | `thread.started`의 thread_id |
| `message` | `{text: string}` | `assistant.content[].text` | `item.completed{agent_message}.text` |
| `tool_call` | `{call_id: string, name: string, args: object}` | `assistant.content[].tool_use{id,name,input}` | `item.started{command_execution}` → name="command_execution", args={command} |
| `tool_result` | `{call_id: string, status: "ok"\|"error"\|"rejected", output: object}` | `user.content[].tool_result{tool_use_id,content,is_error}` + `tool_result_meta.non_execution_kind` | `item.completed{command_execution}{exit_code,aggregated_output}` |
| `approval_request` | `{request_id: string, call_id?: string, name: string, args: object, reason?: string}` | PreToolUse 훅 입력(§2) | (T14: 해당 없음 — 픽스처 근거로 skip) |

설계 주석:
- `tool_result.status`는 T5에서 확정한 `toolResultPayload`와 같은 판별 어휘를
  쓴다(`ok`/`error` + 훅 거부용 `rejected`). `output`은 객체 — 스칼라·문자열
  출력은 T5와 동일하게 `{"value": …}`로 감싼다.
- `call_id`는 도구 네이티브 ID를 그대로 보존한다(Claude `toolu_…`,
  Codex `item_N`). 코어는 불투명 문자열로만 취급한다.
- **매핑하지 않는 네이티브 이벤트**: `system/thinking_tokens`,
  `rate_limit_event`, `assistant.content[].thinking`, `system/init` 이외의
  system subtype. 이유: 정규화 어휘에 대응이 없고 모델 가시 내용도 아니다.
  단 §8의 "중간 이벤트 유실 0" 테스트는 **매핑 대상 이벤트**에 대한 것이며,
  무시 목록은 골든에 명시적으로 기록해 조용한 누락과 구분한다.

---

## 4. raw 의미

- **원본 NDJSON 한 줄의 delimiter(개행) 제외 바이트를 표준 base64로 보존**한다
  (FR-ADP-04). 재인코딩·재정렬·필드 삭제 금지 — 바이트 그대로.
- **한 원본 줄이 여러 정규화 이벤트를 만들면 동일 raw를 각각 첨부한다.**
  근거: `assistant` 한 줄이 `text` + `tool_use` 두 블록을 담을 수 있고
  (픽스처 실재), 어느 정규화 이벤트에서 출발해도 원본을 복원할 수 있어야
  한다(FR-LOG-07). 중복 저장 비용은 raw 압축(부록 A 미결)의 몫으로 남긴다.
- 합성 이벤트(원본 줄 없이 어댑터가 만든 것 — 예: 훅 경유
  `approval_request`)는 T7에서 확정한 규칙대로 빈 base64 `""`.

---

## 5. usage 계산 — 픽스처 증거 기반

Claude는 assistant 메시지마다 usage를 내고 `result`에도 총계를 낸다.
**둘은 일치하지 않는다** (실측):

| 픽스처 | assistant usage 합 (in/cc/cr/out) | result.usage |
|---|---|---|
| 03-multi-tool | (8, 5281, 11106, **25**) | (6, 2814, 9623, **347**) |
| 05-approval-denied | (6, 7026, 3331, **9**) | (4, 3695, 3331, **642**) |
| 07-command-fail | (6, 5133, 7575, **9**) | (4, 2676, 5869, **219**) |

assistant 스트리밍 usage는 부분값이고 output_tokens가 최대 70배 작다.
멀티턴에서는 input 쪽도 합계가 과대(캐시 재계상)다.

**결정 제안**: `subagent/usage`는 **`result` 이벤트에서 한 번만** 방출한다.
assistant별 usage는 합산하지 않는다(이중계상·과소계상 회피). 세부 분해는
raw에 그대로 남는다.

정규화 매핑:
```
input_tokens  = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
output_tokens = output_tokens
```
근거: 셋 다 입력 측 토큰이고 FR-AUD-03의 비용 집계는 입력 총량을 요구한다.
캐시 구분이 필요한 소비자는 raw를 본다.

**overflow**: 세 항의 합은 checked addition으로 계산하고, int64 상한 초과나
음수 값은 **fail-closed(어댑터 프로토콜 위반으로 종료)**다 — T4에서 확정한
`logd`의 usage 합산 정책과 같은 방향. 필드 누락은 0이 아니라 **오류**로
처리한다(조용한 0은 비용 은폐가 된다).

---

## 6. 관측 등급

Claude 어댑터는 `ready.grade = "observable"`로 선언한다(FR-ADP-06).
근거: tool_use/tool_result가 스트림에 실재한다(픽스처 8건 중 7건).

검증(§8): tool call/result가 있는 픽스처에서 **매핑 대상 중간 이벤트의
유실 0** — 골든이 네이티브 tool_use 개수 = 정규화 `tool_call` 개수,
tool_result 개수 = `tool_result` 개수를 단정한다.

---

## 7. 훅 주입 격리 (신규 의존성 없음)

로컬 CLI 도움말 확인 결과:
- `--safe-mode`는 "CLAUDE.md, skills, plugins, **hooks**, MCP servers …
  disabled" — **훅을 끄므로 T9에서는 쓸 수 없다.** (T8 픽스처는 이 플래그로
  녹화됐고, 그래서 픽스처에는 훅 경유 승인 이벤트가 없다 — §8의 스냅샷 설계가
  이를 전제한다.)
- `--setting-sources <sources>` — "Comma-separated list of setting sources to
  load (user, project, local)" → **사용자 설정 상속을 끊는 수단**.
- `--settings <file-or-json>` — 인라인 JSON으로 훅 주입.
- `--session-id <uuid>`, `-r/--resume`, `--fork-session` 존재(B안 대비).

**격리 방식 제안**: `--safe-mode` 대신
`--setting-sources`(사용자/프로젝트 설정 미상속) + `--settings`로 **T9 전용
PreToolUse 훅만** 주입한다. 훅 프로그램은 이 저장소가 빌드하는 작은 도우미
바이너리(`seams/subagent/claudecode/hxapprove`)이며, 어댑터가 env로 넘긴
유닉스 소켓 경로로 판정을 주고받는다.

**신규 의존성 없음**: Agent SDK·MCP 서버·외부 라이브러리를 도입하지 않는다.
소켓·JSON·프로세스 관리는 표준 라이브러리로 충분하다. (도입이 필요해지면
그 시점에 별도 승인 요청.)

**미검증 항목(구현 시 확인)**: `--setting-sources`가 빈 목록을 허용하는지,
그리고 `-p` + 인라인 `--settings` 조합에서 훅이 실제로 발화하는지는 실 세션
실행이 필요하다 — 이는 [H] 범위이므로, 구현 PR에서는 **훅↔어댑터 프로토콜을
fake로 테스트**하고, 실제 발화 확인은 사람의 smoke 절차로 분리한다(§8).

---

## 8. 스냅샷 설계 (네트워크·실 인증 없음)

리뷰 지시대로 세 층을 분리한다.

1. **Claude 8건 골든 대조** — 입력 NDJSON → 정규화 이벤트 **전체**를 골든
   파일과 바이트 비교. 골든은 `testdata/golden/claude-code/NN-*.json`으로
   커밋하고 `make codegen`류의 재생성 없이 손으로 갱신하지 않는다(변경 시
   diff 리뷰). 각 골든에 **무시된 네이티브 이벤트 목록**을 함께 기록해
   조용한 누락과 의도적 무시를 구분한다.
2. **T8 전체 15건 fingerprint** — `make fixtures`에서 매니페스트(§T8 게이트)
   재실행 + 각 파일의 **원본 포맷 fingerprint**(줄 수, 파일 SHA-256,
   top-level type 히스토그램) 대조. 대상 도구의 출력 포맷 변경을 CI에서
   검출하는 층이다(FR-ADP-05). Codex 7건도 이 층에는 포함된다.
3. **Codex 7건은 정규화하지 않는다** — Claude 어댑터가 Codex를 변환하는
   척하지 않는다. 실제 변환은 T14.

**fail-closed 회귀** (전부 픽스처·합성 입력, 네트워크 없음):

| 케이스 | 기대 |
|---|---|
| 64KiB 초과 한 줄 | 오류로 종료 (조용한 절단 금지) |
| 빈 줄 | §5.2 위반 — 오류 |
| 잘못된 JSON | 오류 |
| 미지 native event type | 오류 (조용한 무시 금지 — §3의 무시 목록은 화이트리스트) |
| usage 필드 누락 | 오류 (0 대체 금지) |
| usage overflow/음수 | 오류 |
| approval_response의 미지·중복 request_id | 오류 |
| 판정 타임아웃 | deny (fail-closed) |

**사람 smoke (별도 [H], T9 완료 기준 아님)**: 실 인증으로
`--setting-sources`+`--settings` 조합에서 훅이 실제 발화하는지 1회 확인.
실패 시 §2의 B안 또는 대체 격리 방식으로 재제안한다.

---

## 9. 구현 순서 (승인 후)

1. 순수 parser + Claude 8건 골든 (contracts 변경 없이 가능한 부분까지)
2. contracts 반영 — §2.4 command 분기 + §3 payload 5종 폐쇄, codegen 재생성
3. 독립 실행 어댑터 (`seams/subagent/claudecode`, §5.2 프로토콜)
4. 승인 handshake (`hxapprove` 훅 도우미 + 소켓 프로토콜 + fake 테스트)
5. `make fixtures` 활성화 (§8의 2층)
6. traceability 갱신

---

## 10. 승인 요청 항목

| # | 항목 | 성격 |
|---|---|---|
| 1 | §2.4 `approval_response` command 추가 | **contracts 변경 [H]** |
| 2 | §3 payload 5종 폐쇄 (열린 객체 → 확정) | **contracts 변경 [H]** |
| 3 | §2.2 A안(동기 블로킹 훅) 채택, B안 보류 | 설계 결정 |
| 4 | §5 usage는 result에서 1회, 입력 3항 합산, 누락·overflow fail-closed | 설계 결정 |
| 5 | §7 `--safe-mode` 미사용 + `--setting-sources`/`--settings` 격리, 신규 의존성 없음 | 설계 결정 |
| 6 | §8 3층 스냅샷 분리와 fail-closed 회귀 목록 | 테스트 설계 |
| 7 | §7 미검증 항목의 사람 smoke 분리(T9 완료 기준 제외) | 범위 결정 |
