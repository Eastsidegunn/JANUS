# BLOCKED

구현을 우회하지 않고 멈춘 지점의 기록 (CLAUDE.md 작업 방식).
해소되면 해당 항목을 지우고 태스크를 재개한다.

---

## T10-6 — surface 조립 차단 (2026-08-22)

**현재 차단**: 승인된 Q4의 backend-neutral process broker와 준비/시작 lifecycle이
구현되어 있지 않아, `surfaces/hx`가 FR-SBX-01·FR-ADP-10 및 spawn 선기록 순서를
동시에 만족하도록 조립할 수 없다.

확인된 코드 경로:

- `core/world.Lease.AdapterEndpoint()`는 agent의 start/stdin/stdout/stop/wait를
  제공하는 process broker로 문서화돼 있지만, `seams/world/local.Open()`은 여기에
  approval broker의 `intent`/`next` endpoint를 반환한다.
- `core/world/brokerwire`와 T9 `claudecode` world client가 지원하는 operation도
  approval `intent`/`next`뿐이다. container agent의 stdin/stdout/lifecycle을 host
  adapter에 연결하는 operation이나 transport가 없다.
- 따라서 `claudecode`는 world endpoint가 있어도 `procgroup.Start()`로 Claude를
  host에서 직접 실행한다. 동시에 `local.Open()`은 별도의 agent container를
  시작하므로, surface에서 둘을 조립하면 로그를 만드는 실행은 sandbox 밖 host
  process가 된다.
- `local.Open()`은 lease를 반환하기 전에 proxy·agent container를 create/start한다.
  그러므로 surface가 metadata를 받은 뒤 spawn durable ACK를 기록하고 그 뒤에만
  agent를 시작하라는 T10-6 순서도 현재 `Backend`/`Lease` 계약으로 표현할 수 없다.

이 상태에서 endpoint를 approval 용도로만 넘기거나 host Claude 실행을 유지하는 것은
FR-SBX-01을 형식적으로만 만족시키는 우회다. adapter를 container 안에 넣는 방식도
FR-ADP-10과 승인된 Q4를 위반하므로 사용하지 않는다.

해소 조건:

1. host adapter가 container agent의 start/stdin/stdout/stop/wait를 사용하는
   backend-neutral process broker 계약과 local 구현을 별도 설계·승인한다.
2. `Backend.Open`을 resource preparation과 process start로 분리해, metadata 획득 →
   `subagent/spawn` durable ACK → agent start 순서를 타입/API와 회귀 테스트로
   강제한다.
3. approval relay는 process broker와 별도 request-only endpoint로 유지하고,
   agent에는 host adapter/process broker/Podman socket을 노출하지 않는다.

해소 전에는 T10-6 surface wiring이나 T10-7 통합 workflow를 진행하지 않는다.

---

## T10 — 차단 해소 (2026-08-20)

**해소**: C안 승인 — 통합 테스트는 Linux CI에서 실행하고 로컬은 Fake 백엔드
단위 테스트로 간다. 런타임을 이 머신에 설치하지 않는다.

조사 결과 `ubuntu-latest` 러너에 podman 5.8.4가 이미 있고 rootless와 커널
네이티브 overlayfs가 추가 설정 없이 성립한다. 설치 단계가 없으므로 신규 외부
의존성도 발생하지 않는다. 실측·판단 근거는 `docs/t10-runtime-findings.md`.

macOS 로컬 설치(A·B안)를 택하지 않은 이유는 편의가 아니라 대표성이다. macOS
에서는 어떤 런타임도 Linux VM 안에서 돌고 overlayfs도 VM 내부 커널에서
성립하므로, 거기서 통과한 통합 테스트가 배포 대상을 대표하지 못한다.
