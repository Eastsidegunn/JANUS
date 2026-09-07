# BLOCKED

## T16 SCP-T16-001 — control_mode 값 실측 차단 (2026-09-07)

기준 HEAD: `45eb2c091f75f17e25da7f967117a3865bf1d13b` (main).
스키마 형태 승인은 유지한다. 값 미확정으로 schema/codegen/emitter/audit
구현에는 착수하지 않았다.

- 로컬 설치: `/Users/eastsidegunn/.local/bin/codex`, `codex-cli 0.153.4`.
- `codex --help`: `-a, --ask-for-approval`은 `on-request`, `never`를
  열거한다. `untrusted`가 도움말에 없다는 사실만으로 제거됐다고 단정하지 않는다.
- `codex exec --help`: `-s, --sandbox`는 `read-only`, `workspace-write`,
  `danger-full-access`. 모델 생성 shell command의 sandbox 선택이라고 설명한다.
  exec 자체에는 `-a`가 없으며 글로벌 옵션 위치가 필요하다.
- `codex sandbox -c 'sandbox_mode="read-only"' /usr/bin/true`는
  `sandbox-exec: sandbox_apply: Operation not permitted`로 실패했다.
- `codex -a never exec --ephemeral --json -s read-only`로 임시 파일 생성만
  요청한 프로브는 exit 1, `failed to initialize in-process app-server client:
  Operation not permitted`였다. native command 실행 전에 실패했으므로
  파일 미생성을 정책 enforcement 증거로 사용하지 않는다.

현재 관리형 실행 환경에서는 정책의 양성/음성 대조를 완료하지 못했다.
05 meta의 untrusted 동작 유지 여부도 미확인이다. `container_only`와
`spawn_time_policy` 어느 값도 확정하지 않는다. [H]의 일반 로컬 터미널에서
격리된 임시 디렉터리로 동일 동작의 허용/거부 대조와 untrusted 실 세션을
측정해야 한다. 자격증명은 CI/원격에 보내지 않는다. 좁힘이 확인되면
제안 값을 보고하고 [H] 재확인 전 구현하지 않는다.

## T16-1 — Codex control-mode marker (2026-09-06)

**Blocked pending SCP-T16-001.** The required durable distinction between
Codex spawn-time policy and Claude tool-level approval cannot be represented by
current closed contracts: `subagent/spawn`, `subagent/ready`, tool, and
approval payloads have no such field and use `additionalProperties:false`.
No adapter, contracts, or fixtures were changed. See
`docs/scp-t16-codex-control-mode.md`; after contract approval, resume T16-1.

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
