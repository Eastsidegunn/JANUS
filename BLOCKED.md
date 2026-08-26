# BLOCKED

구현을 우회하지 않고 멈춘 지점의 기록 (CLAUDE.md 작업 방식).
해소되면 해당 항목을 지우고 태스크를 재개한다.

## T10 — 차단 해소 (2026-08-20)

**해소**: C안 승인 — 통합 테스트는 Linux CI에서 실행하고 로컬은 Fake 백엔드
단위 테스트로 간다. 런타임을 이 머신에 설치하지 않는다.

조사 결과 `ubuntu-latest` 러너에 podman 5.8.4가 이미 있고 rootless와 커널
네이티브 overlayfs가 추가 설정 없이 성립한다. 설치 단계가 없으므로 신규 외부
의존성도 발생하지 않는다. 실측·판단 근거는 `docs/t10-runtime-findings.md`.

macOS 로컬 설치(A·B안)를 택하지 않은 이유는 편의가 아니라 대표성이다. macOS
에서는 어떤 런타임도 Linux VM 안에서 돌고 overlayfs도 VM 내부 커널에서
성립하므로, 거기서 통과한 통합 테스트가 배포 대상을 대표하지 못한다.

## T10 lifecycle-stop 회귀 (2026-08-25)

**현재 차단**: PID 1의 graceful SIGTERM 경로와 SIGTERM 무시 후 SIGKILL 경로를
분리한 `stop` / `stop-ignore` 관통 테스트를 추가했지만, Linux rootless Podman에서
두 경로 모두 process broker의 `streamDone` 대기를 30초 소진한다.

관측된 오류는 `process broker output stream drain: context deadline exceeded` 뒤
`stream_end: write ... use of closed network connection`이다. 정상·abnormal·orphan
경로는 최신 반복 실행에서 이 증상 없이 진행됐고, 단계명 진단도 추가했다.

시도한 보완(attach Kill/ClosePipes, stop 전용 강제 StreamEnd)은 로컬 CI에서는
통과하지만 실물 runner에서 문제를 닫지 못했다. 따라서 테스트를 약화하거나
timeout을 늘려 green으로 만들지 않는다. attach/conmon의 pipe 보유와 broker의
stream 종료 순서를 추가로 계측·재설계해야 하며, 원인이 확정되기 전에는 PR #31을
머지하지 않는다.

검증 run: 32826754125, 32832407162, 32832960325, 32833303870,
32833741349, 32834630518, 32835050107, 32835632685, 32836344535,
32837057504 (모두 lifecycle stop 계열 실패).

개정안 `docs/t10-process-broker-amendment.md`의 선택지 (a)를 반영한 구현을
추가했다. stop ACK 뒤 adapter가 output socket을 명시적으로 닫고, broker는
`consumer-gone-after-done`을 정상 종말로, done 이전 이탈을 fatal로 구분한다.
`chunk-forward`·`output-write`·`reader-drain`·`attach-exit`·
`stream-end-write` 단계 표식과 회귀를 로컬에서 검증했으며 `make ci`는 권한 있는
환경에서 exit 0이다. 다만 Linux rootless Podman에서 graceful/stop-ignore 각각
5회 연속 green과 단계별 run ID는 아직 없다. 이 증거가 첨부되기 전까지 T10
lifecycle 차단은 유지한다.
