# BLOCKED

구현을 우회하지 않고 멈춘 지점의 기록 (CLAUDE.md 작업 방식).
해소되면 해당 항목을 지우고 태스크를 재개한다.

---

## T9 — 사람 smoke (2026-08-19 정지)

**사유**: 승인 handshake의 성립 조건(C안: `--setting-sources project,local` +
인라인 PreToolUse 훅에서 훅이 실제로 발화하는지)은 실 자격증명 실행이 필요한
[H] 태스크다. T9 코드 작업은 전부 끝났고, 이 확인 없이는 머지할 수 없다
(제안서 §7.4, 3차 리뷰 승인 조건).

**준비 완료된 것** (PR #16):
- `seams/subagent/claudecode/smoke_test.go` — `smoke` 빌드 태그로 격리된
  실행 하네스. 코어 역할을 대신해 task 전송 → approval_request 수신 →
  deny/allow 응답 → 툴 실행 여부를 파일로 확인한다. 확인점 1·5(사용자 훅·
  managed policy 존재 여부)는 로그로 보고한다.
- `docs/t9-smoke-runbook.md` — 실행 커맨드, 6개 확인점, 판정 기준, 정지 조건,
  확인점 1의 선택적 증명 절차(개인 설정 백업·원복 포함)
- `make lint`가 `go vet -tags smoke`로 하네스 컴파일을 확인(CI 실행은 안 함)

**해소 조건**: 사람이 `go test -tags smoke …`를 1회 실행해 확인점 6개를
충족하고 로그를 PR #16에 기록한다.

**실패 시**: **API key(`ANTHROPIC_API_KEY`)나 `--bare`로 우회하지 않는다** —
3차 리뷰에서 미승인. 정지 후 제안서 §2.2의 B안(defer + resume) 또는 다른
격리 방식으로 재제안한다.

---
## T10 — 예상 차단 (착수 시 확인 필요)

**사유**: 이 개발 머신에 컨테이너 런타임이 없다 — `docker`, `podman` 모두
미설치(2026-08-16 확인). FR-SBX-01은 OCI 컨테이너(rootless 기본) 실행을
MUST로 요구하므로, T10 착수 전에 런타임 선택·설치가 사람의 결정으로
필요하다(신규 도구 도입이므로 승인 대상).

**해소 조건**: 런타임(podman rootless 권장 후보) 승인·설치, 또는 CI에서만
통합 테스트를 돌리는 방식의 승인.
