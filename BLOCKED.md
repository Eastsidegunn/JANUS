# BLOCKED

## T10 lifecycle-orphan 간헐 실패 — escalation (2026-09-23, 재실행으로 덮지 않음)

PR #78(T20) CI에서 `TestWorldIntegration/lifecycle-orphan`이 t15-linux-gate
attempt 5에서만 실패(1~4 pass, 1.84s, VERIFICATION 수준 — 이미지 pull 아님,
orphan 프로세스 reaping 타이밍 레이스). run 35760343569 job 106856530598.

T20 회귀 아님(실물 확인): (1) lifecycle-orphan은 T20 이전 main af5a5ff에
존재, (2) T20의 testagent diff는 순수 additive(삭제줄 0, 기존 모드 무변경),
(3) credentialBroker·AgentEnv는 주입 규칙 있을 때만 활성(이 서브테스트는
주입 없음), 정적 alias는 깨졌으면 1/5 아니라 매번 실패. → T10 선재 flaky.

방침(BLOCKED "재발 시 재실행 금지·stage 데이터 우선" 계승): attempt 5를
재돌려 5/5로 덮지 않는다. root-cause 대상(orphan descendant reaping의
Wait/kill 레이스)으로 남긴다 — T21 후보. [H] (A) 승인(2026-09-23):
escalation은 T20 머지를 막지 않으며 별도 root-cause 태스크로 추적.


## T16 — 해소 기록 (2026-09-12, [H] 지시로 축소)

SCP-T16-001(control_mode)·T16-1 항목 제거. 근거: control_mode enum
[tool_approval|container_only]은 SCP 승인 후 T16-1로 구현·CI green
(docs/traceability.md T16-1 행), 실 codex 세션은 T16-3 [H] smoke PASS
(2026-09-08). `spawn_time_policy` 값 세분은 차단이 아니라 선택적 후속
SCP 후보로 docs/scp-t16-codex-control-mode.md에 이미 기록돼 있다.

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

## T20-② — 컨테이너 게이트에서 업스트림 헤더 수신 실증 (2026-09-23 — 문서화, 우회 안 함)

완료 기준 ②의 "가짜 TLS 업스트림이 `<header>: <prefix><sentinel>` 수신"을
T20 컨테이너 게이트(`surfaces/hx/t20_integration_test.go`)에서 종단 실증하지
못했다. 사유(우회·목업 대신 기록):

1. **scratch 프록시 이미지에 CA 번들 부재.** 통합 프록시 이미지는
   `FROM scratch`(world_integration_test.go buildScratchImage)라 신뢰 루트가
   없다. 주입 재발신은 프록시가 TLS를 개시(RoundTrip https)하므로, 어떤
   업스트림이든 인증서 검증이 핸드셰이크 단계에서 실패한다 → HTTP 헤더가
   전송되기 전에 실패(=sentinel 미전송, fail-closed). 따라서 업스트림이 헤더를
   수신하는 순간을 관측할 수 없다.
2. **isPublicIP가 컨테이너/호스트 IP를 거부.** proxy.go `authorize`는 재발신
   대상 IP가 공인(global-unicast, 비RFC1918)이어야 통과시킨다. 가짜 업스트림을
   같은 podman 네트워크(사설 서브넷)나 호스트 게이트웨이(사설)로 두면 거부된다.
   공인-대역 서브넷+가짜 업스트림 컨테이너로 우회하려면 프록시를 별도 테스트
   네트워크에 사후 연결해 배관 격리 토폴로지를 변형해야 하는데, 이는 실물성이
   아니라 테스트 편의를 위한 토폴로지 변형이라 게이트의 대표성을 훼손한다.
3. **egress 네트워크는 backend 소유.** 테스트가 서브넷/`--add-host`를 지정할
   수 없어 프록시의 DNS·재발신 경로를 하네스에서 제어할 수 없다.

**현 상태**: ②의 헤더 부착 + TLS 재발신 자체는 egressproxy 단위 테스트
`injection_test.go`(가짜 TLS 업스트림, InsecureSkipVerify 루트)에서 이미 종단
실증되어 있다(green). 컨테이너 게이트는 대신 ③(주입 도메인 CONNECT 실물
거부)·④(미선언 도메인 무주입)·⑤(env/inspect/argv/log/audit sentinel·이름
부재)·정적 alias 경유 forward의 프록시 도달(감사 allow)·정리 잔존 0을 실물
실증한다.

**후보 해법(리뷰어 판단)**: (a) T20 전용 프록시 게이트 이미지에 테스트 CA 번들을
넣고, 가짜 업스트림이 주입 도메인 인증서(테스트 CA 서명)를 제공하며, 공인-대역
서브넷 네트워크에 업스트림+프록시를 함께 두어 isPublicIP·DNS·TLS 신뢰를 모두
성립시킨다. (b) 프로덕션 프록시 이미지(CA 번들 보유)를 그대로 쓰고 [H]에서
운영자 통제 하의 실 라우팅 공인 엔드포인트로 관측한다. 어느 쪽도 프로덕션 코드
변경 없이 테스트 아티팩트 계층에서만 구성 가능하나, 상당한 배관과 [H] 실행
검증이 필요해 이번 범위에서 제외하고 기록만 남긴다.
