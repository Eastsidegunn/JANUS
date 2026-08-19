# T9 사람 smoke 실행 절차 — [H: 사람이 실행]

T9 머지의 **필수 조건**이다(제안서 §7.4, 2026-08-18 [H] 승인). 승인 handshake가
실제 Claude 세션에서 동작하는지 확인하기 전에는 구현 완료로 머지할 수 없다.

**에이전트에게 위임하지 않는다** — 실 자격증명을 쓰는 실행이므로 TASKS.md의
[H] 통제 대상이다(T8과 동일).

## 1. 전제

- `claude` CLI가 설치·인증돼 있을 것. 다른 경로면 `HX_CLAUDE_BIN`으로 지정.
- **OAuth 그대로 실행한다.** C안(`--setting-sources project,local` + 인라인 훅)은
  `--bare`를 쓰지 않으므로 기존 인증이 그대로 쓰인다.
- 네트워크·모델 호출이 일어난다(비용 발생). 두 번 실행된다(deny/allow).

## 2. 실행

```sh
cd <저장소 루트>
go test -tags smoke -count=1 -v -timeout 10m ./seams/subagent/claudecode
```

하네스가 코어 역할을 대신한다: pristine 임시 작업공간을 만들고 `task`를 보낸 뒤,
`subagent/approval_request`가 오면 지정 판정(`deny` / `allow`)으로
`approval_response`를 돌려주고 `subagent/done`까지 읽는다. 방출된 모든 이벤트는
§5.2 스키마로 검증된다.

## 3. 확인점 (제안서 §7.4)

| # | 내용 | 하네스 동작 |
|---|---|---|
| 1 | pristine 작업공간에서 사용자 훅 미발화 | `~/.claude/settings.json`의 hooks 유무를 로그로 보고. **없으면 격리는 이번 실행으로 증명되지 않는다** — §5 참조 |
| 2 | 우리 훅 발화 → `approval_request` 방출 | 미방출 시 fatal |
| 3 | **deny 응답 → 툴 미실행** | 작업공간에 `smoke-approved.txt`가 없어야 통과 |
| 4 | **allow 응답 → 실행** | 같은 파일이 있어야 통과 |
| 5 | managed policy/hook 존재 여부 | 알려진 경로 2곳을 확인해 로그로 보고 |
| 6 | 커맨드·결과 기록 | `-v` 로그 전문을 PR에 붙인다 |

## 4. 판정

- **전부 통과**: 로그 전문을 PR #16에 붙이고 Draft를 해제한다. 그다음 최종 [H] 리뷰.
- **확인점 2 실패**(훅 미발화): C안이 성립하지 않는다. **정지하고 재제안**한다.
  - **API key(`ANTHROPIC_API_KEY`)나 `--bare`로 우회하지 마라** — 3차 리뷰에서
    명시적으로 미승인이다.
  - 재제안 후보: 제안서 §2.2의 B안(defer + resume), 또는 다른 격리 방식.
- **확인점 5에서 managed policy 발견**: `--setting-sources`가 통제하지 못하는
  영역이므로 격리 가정이 흔들린다. 로그에 남기고 재제안 여부를 판단한다.

## 5. 확인점 1(사용자 설정 배제) 증명

`TestSmokeApprovalHandshake`는 확인점 2·3·4를 증명하지만 확인점 1은 증명하지
못한다. 사용자 훅이 하나도 없으면 "발화하지 않았다"는 격리의 증거가 아니다.

### 5.1 기본 절차 — 개인 설정을 건드리지 않는다

```sh
go test -tags smoke -count=1 -v -timeout 10m \
  -run TestSmokeUserSettingIsolation ./seams/subagent/claudecode
```

`CLAUDE_CONFIG_DIR`로 사용자 설정 디렉터리를 임시 경로로 옮기고 거기에 마커
훅을 심는다. `~/.claude`은 읽지도 쓰지도 않는다. 두 번 실행한다.

| 실행 | setting-sources | 마커 기대 | 무엇을 증명하나 |
|---|---|---|---|
| A 대조군 | `user,project,local` | **있음** | 기법이 유효함(훅 배선·CLAUDE_CONFIG_DIR 존중) |
| B 실제 | 어댑터 고정값 `project,local` | **없음** | 격리 성립 |

A가 실패하면 B의 결과는 어떤 값이든 증거가 아니다 — 테스트가 그 자리에서
fatal로 멈추고 5.2로 안내한다. B에서 우리 훅의 `approval_request`가 안 보여도
fatal이다(세션이 죽어서 마커가 없는 것을 격리 성공으로 읽지 않기 위해서다).

macOS에서 자격증명은 Keychain에 있어 config 디렉터리 교체와 무관할 것으로
보지만, 실측으로 확인되지 않았다. 인증이 깨지면 A가 실패하므로 잘못된 결론이
나오지는 않는다.

### 5.2 대조군이 실패하면 — 개인 설정 임시 수정 (수동)

`CLAUDE_CONFIG_DIR` 우회가 통하지 않는 버전에서만 쓴다. 개인 설정을 고치므로
자동화하지 않는다.

```sh
cp ~/.claude/settings.json ~/.claude/settings.json.bak   # 반드시 백업
# settings.json의 hooks에 PreToolUse 훅 하나 추가:
#   {"type":"command","command":"touch /tmp/hx-smoke-user-hook-fired"}
rm -f /tmp/hx-smoke-user-hook-fired
go test -tags smoke -count=1 -v -timeout 10m \
  -run TestSmokeApprovalHandshake ./seams/subagent/claudecode
ls /tmp/hx-smoke-user-hook-fired   # 없어야 격리 성립
mv ~/.claude/settings.json.bak ~/.claude/settings.json    # 원복
```

## 6. CI와의 관계

- smoke는 `smoke` 빌드 태그로 격리되어 **CI에서 실행되지 않는다**(네트워크·인증 필요).
- 대신 `make lint`가 `go vet -tags smoke`로 **컴파일만** 확인해 하네스가 썩는 것을 막는다.
