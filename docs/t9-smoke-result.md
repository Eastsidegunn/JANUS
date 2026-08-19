# T9 사람 smoke 결과 — 2026-08-19

`go test -tags smoke -count=1 -v -timeout 10m ./seams/subagent/claudecode`
(claude 2.1.235, C안: `--setting-sources project,local` + 인라인 PreToolUse 훅,
OAuth 유지·API key 미사용)

## 확인점 (제안서 §7.4)

| # | 내용 | 결과 |
|---|---|---|
| 1 | pristine 작업공간에서 사용자 훅 미발화 | **통과** — 별도 테스트로 증명. §확인점 1 참조 |
| 2 | 우리 훅 발화 → `approval_request` 방출 | **통과** (deny·allow 실행 모두) |
| 3 | deny 응답 → 툴 미실행 | **통과** — `smoke-approved.txt` 생성 안 됨 |
| 4 | allow 응답 → 실행 | **통과** — 파일 생성됨 |
| 5 | managed policy/hook 존재 | **없음** — 알려진 경로 2곳 모두 부재 |
| 6 | 커맨드·결과 기록 | 본 문서 |

## 실측 이벤트 순서 (양쪽 실행 동일)

```
subagent/ready → subagent/tool_call → subagent/approval_request
  → subagent/tool_result → subagent/message → subagent/usage → subagent/done
```

## 판정이 실행을 게이트했다는 증거

같은 프롬프트·같은 도구(Write)·같은 대상 경로인데 판정만 달랐다.

- deny: `created=false`, done.result —
  "The Write call was denied (\"테스트 거부\"), so `smoke-approved.txt` was not
  created."
- allow: `created=true`, done.result —
  "`smoke-approved.txt` 생성 완료 — 내용은 `approved` 입니다."

승인 요청의 원본도 확인됐다(`tool=Write`, `call_id=toolu_…`, args에 실제 경로).

## 이 실행이 잡은 결함 (수정 후 재실행하여 통과)

1. **init 순서 규약이 과했다** — 실 세션의 첫 줄은 `system/init`이 아니라
   `rate_limit_event`였다. T8 픽스처 8/8이 init으로 시작한 것은 `--safe-mode`
   녹화 조건의 산물이며 일반화되지 않는다. 무시 대상은 init보다 앞설 수 있고
   매핑 대상만 init 뒤로 강제하도록 수정.
2. **Claude stdin 미종료** — 지시를 argv(`-p`)로 넘기면서 stdin을 열어둬 매
   세션 3초 대기와 경고가 발생했다. `procgroup.CloseStdin()`으로 즉시 종료.

## 확인점 1 — 별도 증명 (2026-08-19)

위 실행으로는 증명되지 않았다. 이 머신의 `~/.claude/settings.json`에 hooks가
없어 배제할 대상 자체가 없었기 때문이다. 전용 테스트
`TestSmokeUserSettingIsolation`으로 따로 증명했다.

```
대조군 통과: user 소스 포함 시 사용자 훅 발화 — 기법이 유효하다
격리 실행 이벤트: subagent/ready → subagent/message → subagent/usage → subagent/done
확인점 1 통과: 사용자 훅 미발화(session-start-fired.txt 부재), 세션은 정상 시작
--- PASS: TestSmokeUserSettingIsolation (2.33s)
```

| 실행 | setting-sources | 마커 | 증명 대상 |
|---|---|---|---|
| A 대조군 | `user,project,local` | 있음 | 기법 자체의 유효성 |
| B 실제 | 어댑터를 실제로 띄움(`project,local` 고정) | 없음 | 격리 성립 |

`CLAUDE_CONFIG_DIR`로 사용자 설정 디렉터리만 임시 경로로 옮긴다. 개인
`~/.claude`은 읽지도 쓰지도 않는다.

대조군을 둔 이유: 마커 부재만 보는 것은 증명이 아니다. 훅이 잘못 배선됐거나
`CLAUDE_CONFIG_DIR`이 무시돼도 똑같이 마커가 없고, 그때 "격리 성공"으로 읽으면
아무것도 증명하지 못한 채 증명했다고 착각하게 된다. A가 실패하면 테스트는 B의
결과를 보지 않고 멈춘다. B에서 `ready`가 없어도 멈춘다 — 세션 사망과 격리를
구분하기 위해서다.

**자격증명을 쓰지 않고 API 호출도 하지 않는다.** `SessionStart` 훅은 첫 API
호출보다 먼저 발화하므로, 임시 config에서 인증이 깨져도(실측:
`authentication_failed`) 판정에 영향이 없다.

### 여기까지 오면서 틀렸던 것

1. **PreToolUse 훅으로 시도** — 툴 호출까지 가야 발화하니 정상 세션이 필요했다.
   `SessionStart`로 바꾸자 인증이 아예 필요 없어졌다.
2. **"자격증명이 Keychain에 있으니 config 디렉터리 교체와 무관하다"** — 내가
   사실로 단언했으나 틀렸다. 실제로는 인증이 깨진다(`Not logged in`). Keychain
   항목 존재만 보고 반대로 읽었고, doctor의 "Not signed in" 신호를 무시했다.
3. **대조군 출력을 앞 1500바이트만 로깅** — 정작 오류가 있는 뒷부분을 버려
   1차 진단을 놓쳤다. 전문 저장 + 꼬리 로깅으로 고쳤다.

## 잔여 위험

`managed-settings.json`은 `--setting-sources`의 통제 밖이다. 이 머신에는 없지만
(확인점 5), 있는 환경에서는 격리 가정이 달라질 수 있다. 배포 대상 환경에서
확인점 5를 다시 봐야 한다.
