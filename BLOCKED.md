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

## T10-final-A — Close 오류 경로의 runtime 자원 누수 (2026-08-23, 해소)

**증상**: run `32587297311`의 orphan lifecycle 뒤 container가 남았다. 원인은
workdir EPERM이 아니라 `Lease.Close`가 caller context·broker shutdown·effect ACK
오류에서 조기 반환해 container/network cleanup을 건너뛴 독립 결함이었다.

**해소**: 커밋 `c986392`에서 모든 단계의 오류를 `errors.Join`으로 누적하고,
취소된 caller context와 분리한 bounded cleanup context로 process stop/wait,
effect drain/ACK, container/network cleanup을 끝까지 시도하도록 고쳤다. 단계별
오류 주입 단위 회귀와 실물 gate의 container/image 및 network 잔존 0 단정을
추가했다. Linux run `32590121367`에서 lifecycle abnormal·stop·orphan 시나리오가
모두 이 단정을 통과했다.

## T10-final-B — lifecycle-stop 오진 (2026-08-23, 해소)

**증상**: run `32587297311`에서 stop helper가 exit 0인데도 `(결과 없음)`과
`status:error`로 끝났다.

**원인과 해소**: test helper의 bare `select {}`는 signal 대기 상태가 아니라 Go
runtime의 `all goroutines are asleep - deadlock`을 일으켜 native done을 방출하기
전에 종료했다. helper를 bounded long sleep으로 바꿔 Podman SIGTERM 경로가 실제로
stop을 관측하게 했다. Linux run `32590121367`에서 lifecycle-stop이 통과했고,
오류 result도 재현되지 않았다.

## T10-final-C — rootless native-overlay work cleanup (2026-08-23, 정지)

**활성 차단**: 정상 시나리오는 파일 변경·egress·승인·output flood까지 수행하지만
host `os.RemoveAll`이 subordinate UID 소유 mode 000 `overlay/work/work`에 진입하지
못해 `EPERM`으로 끝난다. A·B 해소 뒤 Linux run `32590121367`에 남은 실패는 이
한 건뿐이다.

일회성 rootless/native-overlay probe `32590510041`에서 다음을 실증했다.

- host work 삭제 실패, `podman unshare rm -rf -- work` 성공
- whiteout은 host UID/GID 165536의 `rdev=0:0` character device
- `podman unshare rm -rf -- upper` 성공
- 별도 real overlay의 host upper 삭제도 성공: whiteout 소유권과 unlink 권한은
  같지 않고, runner 소유 부모 디렉터리에서 host unlink가 가능함
- state root 밖 sentinel 불변

안전한 exact-path capability, collector ACK 전 upper 보존, 오류 의미론은
`docs/t10-overlay-cleanup-proposal.md`에 분리했다. 해당 개정안 승인 전에는 cleanup
구현이나 Linux gate 단정 변경을 하지 않는다.

해소 조건:

1. 제안서의 lease-bound target 검증과 `podman unshare` cleanup 권위를 승인한다.
2. work cleanup만 구현하고 upper는 T11 durable collector ACK 전까지 보존한다.
3. §4.3의 7개 단정을 하나도 줄이지 않은 `make world-integration`을 Linux CI에서
   다시 green으로 만든다.

관련 run:

- `32586469038`: 긴 application state 아래 Unix socket 경로 상한 발견
- `32586764845`: container stdin이 create 시 열리지 않은 결함 발견
- `32587297311`: Close 누수·stop helper·work cleanup 세 증상 관측
- `32590121367`: Close 누수와 stop helper 해소, work cleanup만 잔존
- `32590510041`: userns work/upper cleanup과 host upper unlink 비대칭 실증
