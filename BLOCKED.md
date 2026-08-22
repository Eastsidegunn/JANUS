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

## T10-final — rootless native-overlay cleanup 전제 반증 (2026-08-23)

**정지**: PR #29의 필수 Linux 실물 게이트 run `32587297311`에서 정상
container 시나리오가 파일 변경·egress·승인·output flood를 수행하고 lease
`Close`에 도달했으나, host 정리가 다음 오류로 실패했다.

```text
world/local: workdir cleanup: openfdat .../overlay/work/work: permission denied
```

승인된 설계는 runner 소유 부모 디렉터리이므로 overlay work 정리가 가능하다고
전제했지만, 실제 rootless native overlay가 만든 `work/work`는 subordinate UID
권한 경계 안에 있어 host `os.RemoveAll`이 진입하지 못했다. `podman unshare rm`
같은 정리 방식을 임의로 도입하면 승인된 lifecycle/권한 설계를 변경하므로
우회하지 않았다.

같은 run에서 test helper의 `select {}`가 Go runtime deadlock으로 종료되어 stop
상태가 error로 관측된 문제도 드러났다. 이는 helper 구현 결함으로 수정 가능하지만,
overlay 정리 설계가 다시 승인되기 전에는 후속 통합 수정·재실행을 진행하지 않는다.

해소 조건:

1. subordinate UID 소유 overlay work/whiteout을 안전하게 정리할 권위와 명령을
   결정하고, 기존 `process stop → effect drain/ACK → mount/network cleanup` 순서를
   보존하는 변경을 승인한다.
2. 정리 실패가 upper 증거 보존과 T11 collector ACK 규약을 침범하지 않음을
   명시한다.
3. stop helper의 bounded blocking 방식을 수정한 뒤 §4.3의 7개 단정을 하나도
   줄이지 않은 `make world-integration`을 Linux CI에서 다시 실행한다.

근거 run:

- `32586469038`: 긴 application state 아래 Unix socket 경로 상한 발견
- `32586764845`: container stdin이 create 시 열리지 않은 결함 발견
- `32587297311`: 위 두 결함 해소 뒤 native-overlay work cleanup 전제 반증
