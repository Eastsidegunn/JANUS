# T16 Codex 실 세션 smoke 결과 — 2026-09-08 [H]

`go test -tags codexsmoke ... TestCodexSmoke` (codex-cli 0.153.4, 사용자 실
macOS 터미널, 인증된 codex).

## 결과: PASS (13.7s)

실 codex 세션의 native 이벤트:
```
thread.started → turn.started → item.completed(agent_message)
→ item.started/completed(command_execution: printf 'ok' > codex-smoke.txt)
→ item.completed(agent_message) → turn.completed(usage)
```
어댑터 정규화(§5.2, 전부 유효):
```
ready → message → tool_call → tool_result → message → usage → done
```
marker `codex-smoke.txt` = "ok" 생성 확인.

## 증명된 것

- **§8-2 Codex 실 세션 child span 툴 콜** — command_execution이 child span의
  tool_call→tool_result로 durable 기록. 실 codex가 파일을 만들었고 어댑터가
  잡았다.
- **T9 교훈 통과** — 실 codex 0.153.4가 골든 0.147.0 어휘와 일치. 버전이
  올랐는데도 어휘 안정. 픽스처 밖 이벤트가 나왔으면 codex.Run이 fail-closed.

## 실 실행이 잡은 하네스 버그 2건 (구현자 환경은 codex 미실행이라 못 잡음)

1. `-a never` — codex exec는 -a 미지원(`unexpected argument -a`). 승인은
   sandbox 모드로 통제. 제거.
2. `CombinedOutput` — codex stderr(`Reading additional input from stdin...`)를
   stdout JSON에 섞어 파서 파괴. stdout만 파서로 분리.

## 인증 — FR-SBX-04 (미해소, Claude와 공통)

codex 인증(OPENAI_API_KEY 또는 codex login 저장 토큰)은 스코프가 좁은 단기
토큰이 아니다. FR-SBX-04는 Claude 경로와 **같은 문서화된 부분 충족**으로 남는다
— 벤더 공통 한계이며 codex 고유가 아니다. control_mode=container_only로 감사
구분됨(SCP-T16-001).

## 잔여

- codex `--sandbox`가 HX 리눅스 컨테이너 안(Landlock)에서 실행을 좁히는가 —
  macOS Seatbelt 실측은 됐으나(PR #68) 컨테이너 안은 리눅스 CI 프로브 몫.
  확인되면 container_only → spawn_time_policy 세분 후속 SCP.
