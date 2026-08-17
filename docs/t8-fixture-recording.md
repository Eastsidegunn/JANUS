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

```sh
CAPTURE=/tmp/t8-captures
mkdir -p "$CAPTURE/claude-code" "$CAPTURE/codex"

# 시나리오 NN마다:
WS=$(mktemp -d /tmp/t8-case-NN.XXXXXX)
cd "$WS"
# (시나리오가 초기 파일을 요구하면 여기서 명시적으로 생성 — §3의 '초기 상태' 열)
```

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
불가능한 시나리오는 meta에 사유를 남기고 건너뛴다). 합계 15개 이상.

## 4. meta.txt 필수 기록

- 도구 정확 버전 (`claude --version` / `codex --version`)
- 실행 커맨드 전체 (플래그 포함, 복사·붙여넣기 가능하게)
- 녹화 날짜, 프롬프트 원문
- **작업공간 초기 상태** (§3의 초기 파일과 내용)
- **CLI exit code**
- **중단 방법** (08번: Ctrl-C 시점/timeout 값)
- **stderr 처리 방식** (별도 파일 경로)

## 5. 커밋 전 비밀 검사 (필수, fail-closed)

검출이든 스캔 오류든 non-zero로 끝나는 게이트다 — `sh check-secrets.sh`로
실행해 **exit 0일 때만** 커밋한다:

```sh
#!/bin/sh
# check-secrets.sh — 검출: exit 1, grep 오류: exit 2, 무검출: exit 0
matches=$(grep -rInE 'sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_|xox[baprs]-|BEGIN [A-Z ]*PRIVATE KEY|eyJ[A-Za-z0-9_-]{10,}\.eyJ' \
  contracts/fixtures/)
status=$?
case $status in
  0) echo '비밀 검출 — 커밋 금지:'; echo "$matches"; exit 1 ;;
  1) echo '비밀 검사 통과'; exit 0 ;;
  *) echo "grep 실행 오류 (exit $status) — 통과로 간주하지 않음"; exit 2 ;;
esac
```

(core/logd redaction 기본 패턴과 동일 집합. 검출 시 해당 녹화를 폐기하고
재녹화한다 — 픽스처는 수정 금지이므로 마스킹 편집도 불가.)

추가로 홈 디렉토리 경로 등 개인 식별 정보가 과하게 남았는지 육안 확인:

```sh
grep -rn "$HOME" contracts/fixtures/ | head
```

## 6. 커밋

```sh
git checkout -b t8/fixtures
mkdir -p contracts/fixtures
cp -R "$CAPTURE/claude-code" "$CAPTURE/codex" contracts/fixtures/
sh check-secrets.sh || exit 1
git add contracts/fixtures/
git commit -m "fixtures: T8 Claude Code·Codex 녹화 N개 시나리오 (FR-ADP-05 전제)"
```

PR 후 T9에서 이 픽스처에 대한 스냅샷 테스트가 `make fixtures`를 실제
대조로 대체한다.
