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

## T10 lifecycle-stop 회귀 (2026-08-25 — 해소)

**해소**: `docs/t10-process-broker-amendment.md`의 선택지 (a)를 구현하고,
실패를 단계별로 계측한 뒤 Linux rootless Podman 관통 게이트를 동일 SHA에서
5회 연속 통과했다. `lifecycle-stop`(graceful SIGTERM)과
`lifecycle-stop-ignore`(SIGKILL escalation) 모두 bounded `Wait`·`streamDone`·
Lease cleanup 및 runtime artifact 0을 매번 단정한다.

구현한 불변식은 다음과 같다:

- container `podman wait`가 종료 권위이며, 종료 관측 뒤 attach reader의 유한
  drain과 attach client reap을 분리한다.
- explicit stop + 완전한 output drain 뒤 attach client를 즉시 kill/reap하고,
  adapter의 durable done 뒤 output/control peer close는
  `consumer-gone-after-done` 정상 종말로 분류한다. done 이전 peer 이탈은 여전히
  `ErrStreamConsumerGone` fatal이다.
- `ExitObserved`와 Stop ACK의 합법적인 선후 경합을 client가 상관하며, ACK가
  terminal frame에 가려져 EOF로 오인되지 않는다. pre-exit control 단절은 계속
  fatal이다.

실패를 숨기지 않고 확인한 단계 증거는 `32933127519`(reader-drain),
`32933493316`(attach-exit), `32933927934`(ACK/terminal 경합)이며, 각 수정 뒤
최종 동일 SHA `62aa0ad`의 Linux run `32934687022` attempts 1–5가 모두 green이다.
`make ci`도 로컬에서 exit 0이다. 이 항목의 차단을 해소하고 PR #31의 구현을
검수 대상으로 전환한다.

## T11 collector — opaque overlay 표현 불일치 (2026-08-27 — 차단)

§2.3 선행 Linux probe `33041950035`에서 제안서의 opaque-directory 표와 다른
실물 표현을 확인해 정지했다. rootless native overlay에서 lower의 `opaque/`
디렉터리를 컨테이너 안에서 통째로 삭제했을 때 host upper에는
`opaque` 경로의 **subuid 소유 character-device whiteout (rdev 0:0)**가
생겼다. opaque xattr 또는 `.wh..wh..opq` marker가 아니었다. 같은 probe에서
rename은 원본 경로 whiteout + 새 regular file, hardlink는 동일 inode 두 경로,
symlink는 upper symbolic link로 관측됐다.

현재 collector 구현은 directory를 가리키는 whiteout을 잘못된 대상으로
거부하고 opaque xattr/marker만 subtree 삭제로 확장하므로, 이 실측을 임의로
정상 처리하도록 바꾸지 않는다. 디렉터리 whiteout을 opaque subtree 삭제로
해석할지, 표와 계약을 어떻게 개정할지 명세 소유자의 재제안·승인이 필요하다.
probe의 최종 host cleanup은 work 디렉터리 subuid 권한으로 실패했지만, 위
upper `lstat/stat` 출력은 수집되어 표현 불일치 판단의 근거로 보존했다.

해소 조건: directory whiteout 의미론과 collector 변경 범위를 승인하는 개정
제안, 그리고 동일 Linux 조건에서 개정 표를 재확인하는 probe run이다.
