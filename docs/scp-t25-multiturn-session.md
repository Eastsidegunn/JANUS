# SCP-T25-001 — 다중 턴 세션: spawn 표시 + send_message/emit 계약 확장

작성 2026-09-30, 리뷰어 세션. 왕복 기록: Rhizome 세션 scratchpad
`janus-session-brief.md`(rev3) / `janus-session-brief-reply.md`.

> **결정: 승인** (2026-09-30, [H] — Rhizome 채널 경유 최종 비준). 역할분담
> 확정: **JANUS = 정보 emit + 명령 실행만, Rhizome = 판단**(예산·idle·종료).
> JANUS 런타임 backstop(budget/idle 강제·max_turns) **신설하지 않음** —
> orphan 무한 실행 위험은 [H]가 명시 수용((a) 선택), 최후 안전선(절대상한·
> 장기무응답 강제종료)은 실제 워크플로우 설계 시 추가. 스키마 수정 커밋은
> T1 조항대로 [H] 리뷰 후 머지.

## 1. 왜 필요한가

Rhizome이 격리 컨테이너의 에이전트 세션을 **다중 턴으로** 구동해야 한다
([H] 갈래 A 확정 — 사람-구동 PTY 아님, attach-PTY 브리프 폐기). 현 world
모드는 one-shot: task가 spawn 시점 `-p <instruction>`으로 고정(T24
ContainerArgv)되고 첫 result 후 종료. 실행 중 세션에 후속 user 메시지를
넣는 채널이 없다.

선택된 형상 — **구조화된 메시지 주입**: claude CLI `--input-format
stream-json`으로 stdin에 후속 user 메시지를 주입. 승인 축(tool_use→relay)·
이벤트 kind(`subagent/message`·`tool_call` 재사용)·audit 의미론 전부
무변경. 신규 이벤트 kind 불요.

## 2. 제안 (1) — `subagent/spawn` payload 다중 턴 모드 표시

spawn 분기는 T10에서 non-additive 폐쇄([H] 2026-08-21 승인)이므로 변경은
SCP 대상. **optional 필드 1개 추가** 제안 (required 무변경,
additionalProperties: false 유지):

| 필드 | 타입 | 의미 |
|---|---|---|
| `session_mode` | enum `oneshot\|multiturn` (optional, 부재 = `oneshot`) | 다중 턴 세션 여부. `multiturn`이면 어댑터가 첫 result 후 stdin을 닫지 않고 send_message 주입을 수용 |

- **하위 호환**: 기존 이벤트·픽스처 전부 유효(optional). 픽스처 무수정.
- **전방 강제**: multiturn 경로에서는 값 기록을 테스트로 의무화
  (SCP-T18-001과 같은 방식 — 스키마는 느슨하게, 경로는 엄격하게).
- 필드명·enum 표기 확정은 [H] 스키마 리뷰 시. codegen 재생성 +
  codegen-drift 게이트.

## 3. 제안 (2) — janus-execution-contract 확장 (Rhizome 소유 스키마)

계약 파일은 Rhizome 임시 소유(T17 결정 1번) — JANUS에 계약 파일을 만들지
않고 본 절을 제안으로 회신한다. 확장 3종:

1. **`send_message{session_id, text}`** (inbound op): 실행 중 multiturn
   세션에 후속 user 메시지 주입. 기존 승인 소켓에 op 다중화(T19 stop
   선례). 비-multiturn 세션·종료 세션에는 결정적 거부.
2. **`stop{session_id}`**: 신규 op 아님 — **기존 T19 stop 재사용**. reason
   enum·권한 분리·멱등 규칙 전부 무변경. Rhizome이 예산/idle 판단 후 stop을
   호출하는 것이 유일한 종료 경로(정상 done 제외).
3. **아웃바운드 emit** (세션별): 호출별 usage(토큰)·last-activity·세션
   상태(running|exited|exit_code). **세션 이벤트 로그의 읽기 전용 사영** —
   usage_in/out·ts·spawn/done에서 파생, 별도 상태 저장소·제2 writer 없음
   (단일 writer·파생상태 재계산 불변식 이행. T18 Rebuild 순수 함수 선례).
   seq 연속, gap 추론 금지.

## 4. 명시적 비범위 ([H] 결정으로 제외 — 구현자는 만들지 말 것)

- JANUS-측 다중 턴 런타임 backstop: 예산 소진 강제 종료, idle deadline,
  max_turns. **판단 주체는 Rhizome**이며 JANUS는 usage/idle 정보를 emit할
  뿐이다.
- 단, **기존 접수 시점 검증은 존치**: T17 BUDGET_INVALID(요청 예산의 병합
  정책 초과 거부)·정책 pin 등 main에 이미 있는 강제는 제거 대상이 아니다.
  이번 결정이 빼는 것은 *신설될 뻔한* 런타임 backstop뿐.
- attach-PTY(원시 터미널) — 브리프 폐기. 별도 요구가 실재하면 재상정.
- 예산 규범 자체는 유지: 세션 총량이 상한(턴별 리셋 금지) — 집행 주체만
  Rhizome.

## 5. 알려진 미해결 (본 SCP 범위 밖, 별도 트랙)

- **D-track: world-모드 approval 데드락** — 다중 턴은 승인 왕복을 유지하므로
  이를 회피하지 못한다(⭐ 회피 주장은 폐기됨). 정면 수리 대상. 재현 조건·
  관측 로그는 [H]/Rhizome이 정리해 JANUS로 전달 예정.
- codex 어댑터의 다중 턴 등가 경로 — claude 우선, codex는 후속.
