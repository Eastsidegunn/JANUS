# BLOCKED

구현을 우회하지 않고 멈춘 지점의 기록 (CLAUDE.md 작업 방식).
해소되면 해당 항목을 지우고 태스크를 재개한다.

---

## T8 — 픽스처 녹화 (2026-08-18 정지)

**사유**: 실 자격증명이 필요한 [H: 사람이 실행] 태스크다. TASKS.md T8과
docs/t8-fixture-recording.md가 에이전트 위임을 명시적으로 금지하므로,
CLI가 설치·인증돼 있더라도 에이전트가 실행하면 통제 위반이다.

**준비 완료된 것** (PR #12, 머지됨):
- `docs/t8-fixture-recording.md` — 격리 녹화 절차, 시나리오 10종 표(초기
  상태 포함), 재현 플래그(Claude 2.1.233 / Codex 0.147.0), meta 필수 항목,
  README 템플릿, 계수·완료 판정 규칙
- `tools/check-fixture-secrets.sh` — 비밀 검사 게이트 (0/1/2 fail-closed)
- `tools/check-fixture-manifest.sh` — README·meta 대응·skip 제외·최소 15건
- 두 게이트의 동작은 `tools/fixturecheck` 테스트로 CI 고정

**해소 조건**: 사람이 가이드대로 녹화해 `contracts/fixtures/`를 담은
`t8/fixtures` 브랜치·PR을 올린다. 두 게이트가 exit 0이고 coverage 5종이
충족되면 T9(Claude Code 어댑터) 착수 가능.

**T8 대기 중 진행 가능한 태스크 없음**: T9는 픽스처가 입력이고,
T10~T14는 순서상 T9 이후다.

---

## T10 — 예상 차단 (착수 시 확인 필요)

**사유**: 이 개발 머신에 컨테이너 런타임이 없다 — `docker`, `podman` 모두
미설치(2026-08-16 확인). FR-SBX-01은 OCI 컨테이너(rootless 기본) 실행을
MUST로 요구하므로, T10 착수 전에 런타임 선택·설치가 사람의 결정으로
필요하다(신규 도구 도입이므로 승인 대상).

**해소 조건**: 런타임(podman rootless 권장 후보) 승인·설치, 또는 CI에서만
통합 테스트를 돌리는 방식의 승인.
