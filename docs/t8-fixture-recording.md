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
    01-simple-text.meta.txt      # 녹화 메타: 도구 버전, 커맨드, 날짜, 프롬프트
    02-single-tool.ndjson
    …
  codex/
    01-simple-text.ndjson
    …
  README.md                      # 녹화 목록 표 (시나리오, 날짜, 도구 버전)
```

- 파일명: `NN-슬러그.ndjson` (NN은 아래 시나리오 번호).
- meta.txt에 최소 기록: 도구 정확 버전(`claude --version` / `codex --version`),
  실행 커맨드 전체, 녹화 날짜, 프롬프트 원문.

## 2. 녹화 커맨드

임시 작업 디렉토리에서 실행한다(개인 저장소·비밀 파일이 없는 곳):

```sh
mkdir -p /tmp/t8-recording && cd /tmp/t8-recording
```

### Claude Code (stream-json)

```sh
claude -p "<프롬프트>" --output-format stream-json --verbose \
  > NN-슬러그.ndjson
```

### Codex (비대화형 실행)

```sh
codex exec --json "<프롬프트>" > NN-슬러그.ndjson
```

주의: 두 CLI 모두 버전에 따라 플래그가 다를 수 있다 — 녹화 전 `--help`로
JSON 스트림 출력 플래그를 확인하고, 실제 사용한 커맨드를 meta.txt에 그대로
남겨라. stderr는 파일에 섞지 않는다(`2>/dev/null` 또는 별도 파일).

## 3. 시나리오 목록 (도구당 8~10개, 합계 15~20)

TASKS.md 요구 분류(정상/툴 다수/승인 요청/에러/중단)를 커버한다.
각 시나리오는 `/tmp/t8-recording` 안에서만 동작하는 프롬프트로 구성한다.

| NN | 슬러그 | 프롬프트 예시 | 검증 포인트 |
|---|---|---|---|
| 01 | simple-text | "1+1은? 숫자만 답해" | 툴 없는 최소 세션, 텍스트 응답 |
| 02 | single-tool | "이 디렉토리의 파일 목록을 보여줘" | 툴 콜 1회 + 결과 |
| 03 | multi-tool | "hello.txt를 만들고 내용을 읽어서 확인해" | 툴 콜 다수(쓰기+읽기), 순서 |
| 04 | edit-file | "hello.txt의 내용을 'world'로 바꿔" | 편집 툴 이벤트 형태 |
| 05 | approval-request | (권한 모드 기본값에서) "상위 디렉토리 /tmp의 파일을 지워봐" | 승인 요청 이벤트 발생 — **실제로 승인하지 말 것** |
| 06 | tool-error | "존재하지 않는 파일 no-such-file.txt를 읽어줘" | 툴 오류의 표현 |
| 07 | command-fail | "exit 42로 끝나는 셸 명령을 실행해" | 비정상 종료 결과 |
| 08 | interrupted | 긴 작업 프롬프트 실행 중 Ctrl-C (또는 timeout 5 …) | 중단 시 스트림 꼬리 형태 |
| 09 | multi-turn | 한 프롬프트에 "먼저 A 하고, 끝나면 B 해" | 복수 스텝, usage 누적 |
| 10 | empty-ish | "" 또는 "." 같은 극단 입력 | 경계 입력의 출력 형태 |

Codex도 같은 번호 체계로 동일 의도의 시나리오를 녹화한다(도구 특성상
불가능한 시나리오는 meta.txt에 사유를 남기고 건너뛴다). 합계가 15개
이상이어야 한다.

## 4. 녹화 시 주의

- **프롬프트에 비밀·개인정보를 절대 넣지 않는다.** 실 API 키는 CLI 인증에만
  쓰이고 출력에 남지 않아야 정상이지만, §5 검사로 반드시 재확인한다.
- 세션 재개·캐시가 섞이지 않게 시나리오마다 새 세션으로 실행한다.
- 파일은 개행 포함 원본 그대로 보존한다 — 후처리·정렬·수정 금지.

## 5. 커밋 전 비밀 검사 (필수)

```sh
grep -rInE 'sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_|xox[baprs]-|BEGIN [A-Z ]*PRIVATE KEY|eyJ[A-Za-z0-9_-]{10,}\.eyJ' \
  contracts/fixtures/ && echo '비밀 검출 — 커밋 금지' || echo '비밀 검사 통과'
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
git add contracts/fixtures/
git commit -m "fixtures: T8 Claude Code·Codex 녹화 N개 시나리오 (FR-ADP-05 전제)"
```

PR 후 T9에서 이 픽스처에 대한 스냅샷 테스트가 `make fixtures`를 실제
대조로 대체한다.
