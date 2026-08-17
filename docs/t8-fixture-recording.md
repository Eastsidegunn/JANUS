# T8 픽스처 녹화 가이드 — [H: 사람이 실행]

대상: FR-ADP-05의 전제. Claude Code와 Codex의 **실제 출력**을 녹화해
`contracts/fixtures/`에 커밋한다. 이 픽스처가 T9(Claude Code 어댑터)·
T14(Codex 어댑터)의 스냅샷 테스트 기준이며, 대상 도구의 출력 포맷 변경을
CI에서 검출하는 근거다.

**이 태스크는 실 자격증명이 필요하므로 에이전트에게 위임하지 않는다**
(TASKS.md T8). 녹화 후 커밋 전 반드시 §5의 비밀 검사를 통과시켜라.
커밋된 픽스처는 이후 수정 금지다(CLAUDE.md — 외부 도구 출력의 스냅샷).

## 1. 디렉토리·명명 규칙

```
contracts/fixtures/
  claude-code/
    01-simple-text.ndjson        # 도구 원본 출력 (한 줄 = 이벤트 하나)
    01-simple-text.meta.txt      # 녹화 메타 (§4)
    02-single-tool.ndjson
    …
  codex/
    01-simple-text.ndjson
    …
  README.md                      # 녹화 목록 표 (시나리오, 날짜, 도구 버전)
```

- 파일명: `NN-슬러그.ndjson` (NN은 아래 시나리오 번호).

## 2. 격리된 녹화 환경

**시나리오마다 새 작업공간**을 만들고, **캡처 출력은 작업공간 밖**에 둔다 —
파일목록·편집 시나리오가 자기 출력물이나 이전 실행 산출물을 보면 안 된다.

캡처 루트도 **실행마다 새로 만든다**. 고정 경로를 재사용하면 이전 실패·중단
녹화가 남아, 일부 시나리오만 재녹화했을 때 stale 파일이 함께 커밋된다.

```sh
CAPTURE=$(mktemp -d /tmp/t8-captures.XXXXXX)   # 실행마다 새 루트
mkdir -p "$CAPTURE/claude-code" "$CAPTURE/codex"
echo "캡처 루트: $CAPTURE"   # 세션 중단 시 이어가려면 이 값을 기록해 둔다

# 시나리오 NN마다:
WS=$(mktemp -d /tmp/t8-case-NN.XXXXXX)
cd "$WS"
# (시나리오가 초기 파일을 요구하면 여기서 명시적으로 생성 — §3의 '초기 상태' 열)
```

재녹화가 필요하면 **캡처 루트를 새로 만들어 전 시나리오를 다시 녹화**하거나,
기존 루트에서 해당 파일을 지우고 다시 녹화한 뒤 §6의 목록 대조로 확인한다 —
어느 쪽이든 커밋되는 것은 "이번에 의도한 녹화 전부"여야 한다.

### Claude Code (stream-json) — 2.1.233 기준 재현 플래그

```sh
claude -p "<프롬프트>" --output-format stream-json --verbose \
  --safe-mode --no-session-persistence --permission-mode manual \
  > "$CAPTURE/claude-code/NN-슬러그.ndjson" 2> "$CAPTURE/claude-code/NN-슬러그.stderr.txt"
echo "exit=$?" # meta에 기록
```

### Codex — 0.147.0 기준 재현 플래그

`/tmp` 작업공간은 Git 저장소가 아니므로 `--skip-git-repo-check`가 필요하다.

```sh
codex exec --json "<프롬프트>" \
  --skip-git-repo-check --ephemeral --ignore-user-config --ignore-rules \
  -s workspace-write -C "$WS" \
  > "$CAPTURE/codex/NN-슬러그.ndjson" 2> "$CAPTURE/codex/NN-슬러그.stderr.txt"
echo "exit=$?"
```

주의: 두 CLI 모두 버전에 따라 플래그가 다를 수 있다 — 녹화 전 `--help`로
확인하고 **실제 사용한 커맨드 전체를 meta에 그대로** 남겨라. stderr는
NDJSON에 섞지 말고 위처럼 별도 파일로 받는다(캡처 디렉토리에 함께 커밋).

## 3. 시나리오 목록 (도구당 8~10개, 합계 15~20)

TASKS.md 요구 분류(정상/툴 다수/승인 요청/에러/중단)를 커버한다.
'초기 상태'는 작업공간 생성 직후 사람이 만들어 두는 파일이다.

| NN | 슬러그 | 초기 상태 | 프롬프트 예시 | 검증 포인트 |
|---|---|---|---|---|
| 01 | simple-text | (빈 디렉토리) | "1+1은? 숫자만 답해" | 툴 없는 최소 세션 |
| 02 | single-tool | `a.txt`("alpha"), `b.txt`("beta") | "이 디렉토리의 파일 목록을 보여줘" | 툴 콜 1회 + 결과 (자기 출력 미포함 — 캡처는 작업공간 밖) |
| 03 | multi-tool | (빈 디렉토리) | "hello.txt를 만들고 내용을 읽어서 확인해" | 툴 콜 다수(쓰기+읽기), 순서 |
| 04 | edit-file | `hello.txt`("hello") | "hello.txt의 내용을 'world'로 바꿔" | 편집 툴 이벤트 형태 (앞선 시나리오에 의존하지 않음) |
| 05 | approval-denied | 빈 sibling `mktemp -d /tmp/t8-outside-NN.XXXXXX` 준비 | "작업공간 밖 <sibling 경로>에 note.txt를 만들어줘" | 승인 요청 이벤트 발생 → **거부**한다. 무해한 생성 요청만 사용 — 삭제·광범위 작업 요청 금지 |
| 06 | tool-error | (빈 디렉토리) | "존재하지 않는 파일 no-such-file.txt를 읽어줘" | 툴 오류의 표현 |
| 07 | command-fail | (빈 디렉토리) | "exit 42로 끝나는 셸 명령을 실행해" | 비정상 종료 결과 |
| 08 | interrupted | (빈 디렉토리) | 긴 작업 프롬프트 실행 중 Ctrl-C 또는 `timeout 5 claude …` | 중단 시 스트림 꼬리 형태 — 중단 방법을 meta에 기록 |
| 09 | multi-step | (빈 디렉토리) | 한 프롬프트에 "먼저 A 하고, 끝나면 B 해" | 단일 턴의 멀티스텝, usage 누적 |
| 10 | empty-ish | (빈 디렉토리) | "." 같은 극단 입력 | 경계 입력의 출력 형태 |

Codex도 같은 번호 체계로 동일 의도의 시나리오를 녹화한다(도구 특성상
불가능한 시나리오는 meta에 사유를 남기고 건너뛴다).

### 시나리오 계수·완료 판정 규칙

파일 개수만으로는 T8이 끝나지 않는다. 모델이 툴을 쓰지 않고 텍스트로만
답해도 파일은 생기기 때문이다. 다음을 모두 만족해야 완료다.

1. **검증 포인트가 실제 NDJSON에 나타난 녹화만** 1건으로 센다.
   예: 02는 툴 콜 이벤트가 실제로 있어야 하고, 05는 승인 요청 이벤트가,
   08은 중단으로 끊긴 스트림 꼬리가 실제로 있어야 한다. 없으면 프롬프트를
   조정해 재녹화한다(그 시도의 meta에 "검증 포인트 미출현 — 재녹화" 기록).
2. **meta만 있고 녹화가 없는 skip은 15건에 포함하지 않는다.**
3. 각 meta에 아래 두 축을 **분리해서** 적는다 (§4):
   - `scenario:` — 무엇을 녹화했는가. §3 표의 슬러그
     (`simple-text`, `single-tool`, `multi-tool`, `edit-file`,
     `approval-denied`, `tool-error`, `command-fail`, `interrupted`,
     `multi-step`, `empty-ish`).
   - `coverage:` — TASKS.md의 요구 분류 중 이 녹화가 커버하는 것.
     `normal`(툴 없는 최소 세션) / `multi-tool` / `approval-denied` /
     `error` / `interrupted` 중 **0개 이상**. 예: `single-tool`과
     `edit-file`은 시나리오로서 필요하지만 요구 분류에는 해당이 없어
     `coverage:` 가 비어 있을 수 있다(그래도 15건에는 포함된다).
4. **`coverage:` 5종은 전체 픽스처(두 도구 합산)에서 각각 최소 1건** 있어야
   한다: `normal`, `multi-tool`, `approval-denied`, `error`, `interrupted`.
5. 검증 포인트 확인은 원본 NDJSON에서 직접 한다 — 예:
   `grep -c '"type":"tool_use"' NN-슬러그.ndjson` 형태로 세고, 사용한
   확인 커맨드와 결과를 그대로 meta에 남긴다.

위 규칙을 만족하는 녹화가 **합계 15개 이상**일 때 T8 완료다.

## 4. meta.txt 필수 기록

- 도구 정확 버전 (`claude --version` / `codex --version`)
- 실행 커맨드 전체 (플래그 포함, 복사·붙여넣기 가능하게)
- 녹화 날짜, 프롬프트 원문
- **작업공간 초기 상태** (§3의 초기 파일과 내용)
- **CLI exit code**
- **중단 방법** (08번: Ctrl-C 시점/timeout 값)
- **stderr 처리 방식** (별도 파일 경로)
- **`scenario:`** — §3 표의 슬러그 (무엇을 녹화했는가)
- **`coverage:`** — 커버하는 요구 분류 0개 이상
  (`normal`/`multi-tool`/`approval-denied`/`error`/`interrupted`)
- **검증 포인트 확인 결과**: 원본 NDJSON에서 실제로 확인한 커맨드와 그 출력
  (예: `grep -c '"type":"tool_use"' 02-single-tool.ndjson` → `3`)

meta 예시:

```
tool: claude-code 2.1.233
command: claude -p "이 디렉토리의 파일 목록을 보여줘" --output-format stream-json --verbose --safe-mode --no-session-persistence --permission-mode manual
date: 2026-08-18
prompt: 이 디렉토리의 파일 목록을 보여줘
workspace-initial: a.txt("alpha"), b.txt("beta")
exit-code: 0
interrupt: 해당 없음
stderr: 02-single-tool.stderr.txt (별도 파일)
scenario: single-tool
coverage:            # 요구 분류 해당 없음 — 15건 계수에는 포함
verify: grep -c '"type":"tool_use"' 02-single-tool.ndjson → 1
```

## 5. 커밋 전 비밀 검사 (필수, fail-closed)

검사기는 저장소에 있다: **`tools/check-fixture-secrets.sh <디렉토리>`**.
검출이든 스캔 오류든 non-zero로 끝나며, **exit 0일 때만** 커밋한다.

```sh
tools/check-fixture-secrets.sh "$CAPTURE"   # 복사 전 캡처 루트에서 먼저
```

| exit | 의미 | 조치 |
|---|---|---|
| 0 | 무검출 | 커밋 진행 |
| 1 | 비밀 검출 | 해당 녹화 **폐기·재녹화** (픽스처 수정 금지이므로 마스킹 편집 불가) |
| 2 | 사용법 오류·대상 부재·grep 실행 오류 | **통과로 간주하지 않음** — 원인 해결 후 재실행 |

패턴은 core/logd redaction 기본 집합과 동일하며, 3분기 동작은
`tools/fixturecheck`의 테스트로 CI에서 고정된다.

추가로 홈 디렉토리 경로 등 개인 식별 정보가 과하게 남았는지 육안 확인:

```sh
grep -rn "$HOME" contracts/fixtures/ | head
```

## 5.5. README.md 작성 (필수)

캡처 루트에 `README.md`를 만든다 — 커밋 전 매니페스트 검사가 존재를 확인한다.

```markdown
# T8 픽스처 녹화 목록

녹화일: 2026-08-__ / 도구: claude-code 2.1.233, codex 0.147.0

| 파일 | scenario | coverage | 검증 포인트 확인 |
|---|---|---|---|
| claude-code/01-simple-text.ndjson | simple-text | normal | tool_use 0건 확인 |
| claude-code/02-single-tool.ndjson | single-tool | — | tool_use 1건 |
| … | | | |

## coverage 충족 (§3 규칙 4)
- normal: claude-code/01
- multi-tool: claude-code/03
- approval-denied: claude-code/05
- error: claude-code/06, codex/06
- interrupted: claude-code/08

## meta-only skip (녹화 없음 — 15건 미포함)
- codex/05-approval-denied.meta.txt — 사유: (도구 특성상 승인 이벤트 없음 등)
```

## 6. 커밋

복사 대상이 **비어 있는지 fail-closed로 먼저 확인**한다 — 이전 시도의
잔여 픽스처 위에 덮어쓰면 stale 파일이 섞여 들어간다.

```sh
git checkout -b t8/fixtures

# 1) 대상이 비어 있어야 한다 (없거나 빈 디렉토리만 허용)
if [ -e contracts/fixtures ] && [ -n "$(ls -A contracts/fixtures)" ]; then
  echo 'contracts/fixtures가 비어 있지 않음 — 잔여물 확인 후 정리하고 다시 실행' >&2
  exit 1
fi

# 2) 캡처 루트에서 비밀 검사 + 매니페스트 검사 (둘 다 exit 0일 때만 진행)
tools/check-fixture-secrets.sh "$CAPTURE" || exit 1
tools/check-fixture-manifest.sh "$CAPTURE" 15 || exit 1

# 3) 복사 후 대상에서도 재검사
mkdir -p contracts/fixtures
cp -R "$CAPTURE"/. contracts/fixtures/
tools/check-fixture-secrets.sh contracts/fixtures || exit 1
tools/check-fixture-manifest.sh contracts/fixtures 15 || exit 1

# 4) 복사 목록이 캡처와 일치하는지 대조 (누락·stale 검출)
diff -r "$CAPTURE" contracts/fixtures || { echo '캡처와 커밋 대상이 다름' >&2; exit 1; }

git add contracts/fixtures/
git commit -m "fixtures: T8 Claude Code·Codex 녹화 N개 시나리오 (FR-ADP-05 전제)"
```

PR 본문에 §3 계수 규칙의 충족 근거(분류별 건수, 검증 포인트 확인 결과)를
요약해 남긴다.

PR 후 T9에서 이 픽스처에 대한 스냅샷 테스트가 `make fixtures`를 실제
대조로 대체한다.
