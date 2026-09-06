# T16 제안서 — Codex 어댑터 완성

상태: 구현 전 제안. 대상은 FR-ADP-09와 §8-2의 Codex 실 세션 종결 조건이다.
Codex `exec --json`의 실제 녹화 범위와 Claude 경로의 승인 계약을 섞지 않는다.
현재 저장소의 `seams/subagent/codex`는 순수 parser와 7개 유효 골든을 제공하며,
독립 실행파일·실 세션 smoke·Codex용 동기 승인 transport는 아직 없다.

## Q1. 승인 의미론

T8 Codex 녹화는 다음을 보여준다.

```text
thread.started → turn.started → item.started/completed → turn.completed
```

`item.started(command_execution)`는 실행 전 알림일 뿐 승인 대기가 아니다. `05`
메타데이터에는 `codex exec --json`이 승인 UI를 표면화하지 않으며 `-a untrusted`
에서도 승인 화면이 없고 `/tmp` 쓰기가 실행됐다고 기록되어 있다. 따라서 Codex
비대화형 경로에 Claude의 PreToolUse→approval_request→approval_response 등가물이
있다고 가정하지 않는다.

### 선택지

| 안 | 선택 | 비용·실패 모드 |
|---|---|---|
| (a) spawn-time 정책 + 컨테이너 격리로 강등 | **선택 제안** | 툴별 동기 승인은 포기한다. HX의 병합 정책을 Codex의 실제 sandbox/approval 플래그로 번역하고, overlay·egress·프로세스 경계가 효과를 제한한다. FR-POL-05의 툴별 handshake는 Codex에서 부분 충족으로 남는다. |
| (b) 대화형/다른 Codex 모드의 훅 사용 | 보류 | 현재 픽스처에는 근거가 없다. 실제 Codex 설치본과 공식 동작을 [H]가 확인하기 전에는 지원한다고 주장하지 않는다. |
| (c) 성공한 command item을 승인으로 간주 | 탈락 | 실행 후 알림을 사후 승인으로 바꾸며 FR-POL-05와 fail-closed 원칙을 위반한다. |

spawn-time 강등에서는 `ApprovalManual`을 허용 가능한 실행으로 바꾸지 않는다.
수동 승인이 필요한 프로파일인데 Codex native approval 경로가 관측되지 않으면
어댑터는 command effect 전에 `ErrApprovalUnavailable`을 내고 명시적
`subagent/done{status:error}`로 종료한다. `ApprovalAuto`는 프로파일이 명시한
경우에만 사용하며, 그 경우에도 컨테이너 격리와 egress 정책은 그대로 적용한다.
Codex가 자기 sandbox 정책으로 명령을 거부하면 해당 native
`item.completed(command_execution, status=failed)`와 `turn.completed`를
`subagent/tool_result{status:error}` 및 최종 `done{error}`로 매핑한다. 이를
`approval_request`로 위조하지 않는다.

**FR-POL-05 Codex 충족 수준은 [H] 승인 항목이다.** [H]는 (a)의 spawn-time
강등을 수용하거나, 실제 native approval 근거가 발견되면 별도 transport 설계와
명세 판단을 요청해야 한다. 어느 경우에도 부분 충족을 완전 충족으로 표시하지
않는다.

## Q2. 독립 어댑터 실행파일과 플래그

`seams/subagent/codex/cmd/codex`를 Claude cmd 구조와 같은 독립 실행파일로
추가한다. 어댑터는 stdin의 §5.2 `task`를 받고, Codex native stdout JSON을
파싱해 stdout의 §5.2 NDJSON으로 내보내며 진단은 stderr로만 보낸다. `none`
분기의 기존 host procgroup 경로와 `local-podman` ProcessEndpoint 분기를
어댑터 계약에서 혼동하지 않는다.

고정해야 할 native 명령은 다음 표의 항목으로 제한한다.

| 항목 | 현재 근거 | 구현 판단 |
|---|---|---|
| Codex 버전 | T8 meta: `codex-cli 0.147.0` | 정확한 버전과 실행파일 fingerprint를 smoke에서 기록한다. |
| JSON 출력 | 픽스처가 `codex exec --json`으로 녹화됨 | `--json`을 고정한다. |
| 승인/샌드박스 플래그 | 픽스처 meta에 `-a untrusted` 승인 UI 부재만 기록 | 실제 설치본의 `codex --help`와 smoke에서 지원되는 정확한 spelling을 [H]가 확인한다. 확인 전 `--sandbox`나 임의 flag를 지어내지 않는다. |
| 프로파일 번역 | HX의 egress·workspace·approval mode | 확인된 flag 집합으로만 번역하고, 표현할 수 없는 제한은 spawn을 거부한다. 자동 완화나 host fallback은 없다. |

실행 이미지와 Codex 인증은 T15 world 기판을 재사용한다. Codex binary,
adapter binary, approval endpoint, Podman socket을 agent 컨테이너에 mount하지
않는다. API key나 장기 credential을 로그·raw·metadata·frame에 넣지 않으며,
실제 인증 방식과 만료 처리는 [H] smoke 전까지 확정된 것으로 표시하지 않는다.

## Q3. 이벤트 매핑과 경계

파서는 이미 다음 매핑을 골든 7건에서 검증한다.

| Codex native | HX 이벤트 | 의미 |
|---|---|---|
| `thread.started` | `subagent/ready` | child span에서 첫 이벤트 |
| `turn.started` | 없음 | 순서 검증 후 disposition만 남김 |
| `item.started(command_execution)` | `subagent/tool_call` | 실행 전 알림이며 승인 아님. Auto일 때만 effect 진행 |
| `item.completed(command_execution)` | `subagent/tool_result` | completed/failed와 exit code를 fail-closed 매핑 |
| `item.completed(agent_message)` | `subagent/message` | 텍스트 보존 |
| `turn.completed.usage` | `subagent/usage` | checked usage projection |
| `turn.completed` 또는 중단 | `subagent/done` | 성공·오류·stopped를 결정적 문구로 종료 |

어댑터가 추가로 소유할 것은 task 송신, ready-first/done-terminal 경계, stop 시
프로세스 종료와 done 보존, native 비정상 종료의 오류 매핑, 그리고 child span
귀속이다. spawn 이벤트와 durable ACK는 surface/world가 소유하므로 Codex
parser가 직접 기록하지 않는다. native 승인 이벤트가 없으므로 parser가
`approval_request`를 합성하지 않는다.

## Q4. 실 세션 검증과 증거 경계

7개 골든(01, 02, 03, 04, 06, 07, 08)은 `codex-cli 0.147.0` 녹화 조건의
산물이다. T9에서 init 순서가 픽스처와 실 세션에서 달랐으므로 골든만으로
Codex 실행을 완료 표시하지 않는다.

[H] smoke는 Linux rootless Podman 또는 명세 소유자가 승인한 동등한 실행
환경에서 다음을 원문 값 없이 기록한다.

1. `codex --version`과 `codex --help`의 실제 버전·sandbox/approval flag 및
   `exec --json` 이벤트 순서가 골든과 일치하는지, 차이가 있으면 어떤 native
   라인이 추가됐는지.
2. task가 실제 container 안에서 실행되고 host adapter/process/approval
   socket과 Podman socket이 보이지 않는지.
3. spawn-time profile이 workspace·egress·쓰기 범위를 실제로 좁히는지,
   허용/금지 도메인과 IP literal 시도가 proxy/audit 경계를 지키는지.
4. command execution이 child span의 tool_call/tool_result로 기록되고,
   manual profile에서 approval path 미관측 시 실행을 허용하지 않는지.
5. Codex 인증의 종류와 수명 경계. 장기 API key를 저장·로그·CI secret으로
   만들지 않으며, access token/환경 입력을 쓰는 경우에도 redaction과 만료
   오류를 Claude 경로와 같은 수준으로 단정한다.

실 세션에서 픽스처 밖 native 이벤트가 나오면 조용히 무시하거나 fixture를
수정하지 않는다. parser 계약을 확장할 필요가 있으면 별도 제안과 회귀를
먼저 만든다. 승인 UI가 없다는 사실이 유지되면 manual은 `ErrApprovalUnavailable`
과 `done{error}`가 성공이며, 그것을 승인 handshake 성공으로 보고하지 않는다.

## Q5. §8-2 종결 조건

Codex는 다음을 모두 충족해야 §8-2의 Codex 행을 닫는다.

- 골든 7건과 독립 cmd 어댑터가 같은 §5.2 계약을 유지한다.
- 실 Codex 세션에서 실제 command execution 하나 이상이 child span의
  tool_call→tool_result로 durable 기록된다.
- spawn-time 강등을 선택하면 그 프로파일과 컨테이너 경계가 실제로 효과를
  제한하며, manual에서 native approval 부재가 실행 허용으로 변하지 않는다.
- 실 세션의 인증·sandbox flag·이벤트 차이와 사용 커맨드·결과 전문(비밀 제거)이
  [H] 기록으로 남는다.

### [H] 조건부 승인 (2026-09-06) — 강등을 숨기지 않고 보상한다

FR-POL-05 Codex 강등을 수용하되 두 조건을 종결 기준에 추가한다.

1. **통제 강도를 로그에 명시한다.** Codex 세션은 툴별 동기 승인이 아니라
   spawn-time 정책으로 통제된다는 사실이 이벤트에 남아야 한다. `hx audit`이
   "이 command execution은 부모가 개별 승인한 것이 아니다"를 Claude 세션과
   구분할 수 있어야 한다. 강등이 로그에 안 보이면 감사가 Codex 세션을 Claude
   세션과 똑같은 통제로 오독한다 — "안 잡힘"이 "안 함"으로 읽히는 것과 같은
   실패다. (contracts 변경이 필요하면 SCP로 제안만, 기존 필드로 표현 가능하면
   그 방식을 적시)
2. **효과 평면 관측 완전성을 Claude보다 엄격히 요구한다.** Claude는 승인이
   툴 실행을 사전에 막지만 Codex는 못 막으므로, 사후 효과 관측(fsdiff·egress)이
   유일한 안전망이다. Codex 세션에서 effect_observation이 incomplete이면
   Claude보다 위험하다(사전 게이트 부재). §8-2 Codex 종결은 실 세션에서 effect
   평면 관측이 완전함을 함께 단정한다.

이 둘은 강등의 대가다: 약함을 인정하는 데서 멈추지 않고, 그 약함을 관측
가능하게 만들고(1) 다른 층으로 보상한다(2). T12의 `effect_observation` 명시와
같은 정신이다.

Claude 경로가 무자격증명 Linux 게이트 후 [H] 실 세션으로 넘어간 것과 같은
순서를 적용한다. T10/T11/T12의 컨테이너·egress·relay·audit 기판을 Codex의
실 세션 증거로 재사용할 수 있지만, Codex 자체의 native 이벤트와 승인 수준은
별도로 검증한다. 골든 통과만으로 §8-2 Codex 실 세션을 충족했다고 표시하지
않는다.

## 승인 요청

| 항목 | 승인 주체 | 판단 요청 |
|---|---|---|
| Codex `exec --json` 독립 어댑터와 T8 골든 범위 | T16 구현자/리뷰어 | parser의 7개 유효 시나리오와 ready/done/stop 경계를 cmd 실행파일에 연결 |
| Codex spawn-time sandbox/approval flag의 정확한 버전·문법 | [H] | 설치본 `codex --version/--help` 실측 후 고정. 확인 전 지원 주장 금지 |
| **FR-POL-05 Codex 부분 충족 방식** | **[H] — 조건부 수용 (2026-09-06)** | (a) spawn-time 강등 수용. 단 두 조건: 강등을 로그에 명시(감사 구분 가능), 효과 평면 관측 완전성을 Claude보다 엄격히 요구. 완전 충족으로 표기하지 않음. §Q5 참조 |
| Codex 인증·자격증명 경계 | [H] | 장기 API key/refresh 반입 금지, 실제 인증 방식·수명·redaction smoke 범위 승인 |
| §8-2 Codex 실 세션 종결 | [H] | child span tool call 기록, 정책 강등 동작, 컨테이너 격리 및 픽스처 밖 이벤트 기록이 모두 있어야 닫음 |

