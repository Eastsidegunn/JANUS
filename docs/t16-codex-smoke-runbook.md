# T16 Codex smoke runbook [H]

실행은 인증된 로컬 환경에서만 한다. 토큰 값은 출력·로그·저장소에 기록하지 않는다.

1. Codex 설치와 인증을 준비하고 `codex --version`을 확인한다.
2. 플래그 실측: `codex exec --help`에서 `--json`, `--ephemeral`, `--skip-git-repo-check`, `--ignore-user-config`, `-s`가 존재하는지 (codex exec는 -a 미지원 — 승인은 sandbox 모드로 통제) 확인한다.
3. 실행 명령:

```sh
go test -tags codexsmoke -count=1 -v -timeout 10m -run TestCodexSmoke ./seams/subagent/codex
```

하네스는 버전, native 출력 꼬리, 정규화 kind 순서를 기록하고 ready 선행·done
종결·§5.2 검증·marker 파일을 단정한다. 픽스처와 native 순서가 다르거나 미지
이벤트가 나오면 `Run`이 fail-closed하여 smoke가 실패로 드러난다 — 이것이
실 codex(0.153.4)와 골든(0.147.0 녹화) 차이를 잡는 방식이다.

**인증은 하네스가 관찰하지 않는다.** codex 세션 smoke는 codex가 이미 인증된
것을 전제로 실행할 뿐, 인증 방식·수명을 introspect하지 않는다. FR-SBX-04
판단을 위해 [H]가 아래를 수동으로 확인해 기록한다(값·토큰 미기록):

4. codex 인증 방식을 확인한다 — `OPENAI_API_KEY` 환경변수(장기 API key)인지,
   `codex login` 저장 토큰인지. 어느 쪽이든 스코프가 좁은 단기 토큰이 아니면
   FR-SBX-04는 Claude 경로와 **같은 문서화된 부분 충족**으로 남는다(장기
   자격증명의 컨테이너 반입 문제). 이는 codex 고유가 아니라 벤더 공통 한계다.

실패 시 우회하지 말고 native 전문은 로컬 임시 산출물로 보존한 뒤 [H] 검토와 BLOCKED 절차를 따른다.
