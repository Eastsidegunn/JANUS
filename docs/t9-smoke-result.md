# T9 사람 smoke 결과 — 2026-08-19

`go test -tags smoke -count=1 -v -timeout 10m ./seams/subagent/claudecode`
(claude 2.1.235, C안: `--setting-sources project,local` + 인라인 PreToolUse 훅,
OAuth 유지·API key 미사용)

## 확인점 (제안서 §7.4)

| # | 내용 | 결과 |
|---|---|---|
| 1 | pristine 작업공간에서 사용자 훅 미발화 | **미증명** — 이 머신의 `~/.claude/settings.json`에 hooks가 없어 배제할 대상 자체가 없었다. §잔여 위험 참조 |
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

## 잔여 위험 — 확인점 1

`--setting-sources project,local`이 사용자 설정을 실제로 배제하는지는 이번
실행으로 증명되지 않았다(배제할 사용자 훅이 없었다). 증명하려면
`docs/t9-smoke-runbook.md` §5의 임시 마커 훅 절차(개인 설정 백업·원복)를
수행해야 한다. 이는 개인 설정 수정이므로 자동화하지 않으며, 수행 여부는
사람의 판단이다.

영향 범위: 사용자 훅이 있는 환경에서 그것이 함께 발화하면 격리 가정이 약해진다.
다만 (a) 우리 훅은 정상 발화했고 (b) managed policy는 이 머신에 없으며
(c) 작업공간은 pristine이라 project/local 설정도 없다.
