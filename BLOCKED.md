# BLOCKED

구현을 우회하지 않고 멈춘 지점의 기록 (CLAUDE.md 작업 방식).
해소되면 해당 항목을 지우고 태스크를 재개한다.

## CI 간헐 실패 2건 — 처분 완료 (2026-09-03)

run 33542453242 (동일 SHA 2/2 실패)의 두 실패를 다음과 같이 닫았다.

1. **T9 stop 순서 — 해소.** 원인: stop 신호와 native 출력의 실제 경합에서
   테스트가 한쪽 결과(missing_result)만 계약인 양 단정. T9 계약은
   status=stopped 와 deny-선행만 요구한다. fake claude 에 재생 게이트를
   넣어 경합 양방향을 각각 결정적으로 고정한 테스트 2개로 대체(약화가
   아니라 비결정 1개 → 결정 2개). 동일 SHA 84dda940 에서 5회 연속 green
   (run 33624178125 attempts 1–5).
2. **T10 broker backpressure(control read: EOF) — 감시 상태.** 계측 후
   8회 무재현. stage·lifecycle 표식이 main 에 상주하므로(PR #47) 재발 시
   어느 브랜치에서든 원인 단계가 스스로 기록된다. **재발하면 즉시 선결로
   승격하고, 재실행으로 덮지 않는다. stage 데이터 확보가 첫 행동이다.**

T13-4 선결 근거였던 "red 원인 귀속 불가"는 계측 상주로 해소됐다.
**T13-4 착수 가능.**

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

## T11 collector — opaque overlay 표현 (2026-08-27 — 해소)

후속 Linux probe `33049557686`에서 세 표현을 확인했다. 디렉터리 삭제 후
재생성은 `user.overlay.opaque="y"` 디렉터리와 재생성된 파일로, 디렉터리를
파일로 대체하면 regular 파일 하나로, 자식 일부 삭제는 디렉터리 안의
subuid 소유 character-device whiteout (`rdev 0:0`)로 나타났다. collector는
opaque 디렉터리와 directory-path whiteout을 baseline의 모든 잎 삭제로
전개하고, 재생성된 경로는 제외하며, 디렉터리→파일 대체를 삭제+추가로
표현한다. probe 표와 구현이 일치하므로 차단을 해소했다.
