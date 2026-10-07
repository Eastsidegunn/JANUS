# SCP-T26-001 — egress proxy 선언 게이트웨이 핀 예외 (world-config `egress_pins`)

작성 2026-10-02, 리뷰어. 근거: deployment host에서 수행한 수동 smoke — 모든 턴이
claude-code 합성 에러 "Request timed out"으로 종료, 원인은 egress proxy의 설계된 거부.

> **결정: 승인** (2026-10-02, 승인 주체: 명세 소유자 로컬 지시(신원 미검증 표기) — 지시가
> Rhizome 채널 경유로 전달됨, 리뷰어가 사실대로 기록). 선행 조건
> 충족: §1.1 도달성은 deployment host의 Podman 4.x/netavark 전환으로 해소, §6 실물 검증 ②는
> 충족(collector/egress deny 기록 — CGNAT 가드 발화·audit 정상 실증). 구현은 JANUS 구현자/
> 리뷰어 분리 규율로, 코드 변경은 PR·CI green 후 머지.
> contracts/·이벤트 스키마·Rhizome 계약 무변경. 변경은 world-config 형식(JANUS 소유
> 운영자 설정)과 FR-SBX-03 가드의 선언적 예외 의미론이다.

## 1. 왜 필요한가

`seams/world/local/egressproxy/proxy.go` `authorize`는 allowlist 통과 후 **DNS 해석 결과를
`isPublicIP`로 검사**하고, `isPublicIP`는 private/loopback/link-local/multicast에 더해
**RFC 6598 100.64.0.0/10(CGNAT 대역 — 사설 오버레이 네트워크가 흔히 쓰는 대역)을 명시 거부**한다(327~336행). DNS 리바인딩으로
내부 호스트에 닿는 것을 막는 FR-SBX-03 가드다.

T23은 벤더 게이트웨이(CLIProxyAPI)를 "운영자가 JANUS 밖에 배치"로 정의했다. 그런데 이 가드
아래서는 게이트웨이가 **공개 인터넷의 공개 IP에 있어야만** 프록시를 통과한다. 사설망이나 사설 오버레이 네트워크(CGNAT 대역)에 있는 운영자
게이트웨이(`<gateway-ip>:<port>`)는 전부 거부된다. 즉:

- T23 완료 기준 (a)의 기존 실증은 `HX_SKIP_EGRESS_PROXY=1`(프록시 우회, deployment host 로컬 패치)
  상태였다 — FR-SBX-03 MUST가 꺼진 증거라 (a)는 **미증명**이다.
- 프록시 ON으로는 현 토폴로지에서 모델 경로가 열릴 수 없다 → T25 다중 턴 실증 불가.

명세 소유자는 프록시 우회가 아니라 **격리를 켠 채 증명**하기를 결정했다(B). 대안 A(게이트웨이 공개
배치)는 JANUS 무변경이지만 구독 게이트웨이를 인터넷에 노출하는 영구적 자세 변경이라 기각.

### 1.1 선행 조건 — 본 SCP만으로는 프록시-ON이 돌지 않는다
deployment host에서 agent 컨테이너는 `hx-<span>-internal`에만, proxy는 `hx-<span>-egress`에만 붙어
**공유 네트워크가 없었다**(agent→proxy alias EAI_AGAIN, →internal address EHOSTUNREACH). 그래서
위 smoke의 "Request timed out"은 CGNAT 거부가 아니라 **프록시 미도달**이었고, collector/egress
이벤트 0건도 그 때문이다(audit 경로 결함 아님). local.go:554의 설계는 proxy를
`--network internal --network egress` 양쪽에 붙이지만, **Podman 3.4.x의 `--network`는
단일 플래그라 반복 시 마지막 값만 남는다**(4.0+에서 반복 가능). CI(Podman 5.x)의 T10 게이트는
agent→proxy→egress 관통을 증명하므로 설계 결함이 아닌 deployment host 환경 결함이다.
→ 프록시-ON 종단은 **(1) deployment host 도달성 수리(Podman 4.x+ 업그레이드 권고, 또는 로컬 패치) 선행
+ (2) 본 SCP** 둘 다 필요하다. (1)은 main 코드 과제가 아니며(3.4 호환을 main에 들이지 않는
기존 결정 유지) 명세 소유자의 환경 결정 사항. (1) 완료 판정: proxy Networks 2개 + 실물 검증 ②가
403 + collector/egress deny 1건(가드 발화 확인).

## 2. 현황 사실 (코드)

- 사이드카 = `hxegressproxy` 컨테이너 이미지(world-config `proxy_image` digest 핀). 인자:
  `--listen`, `--audit-socket`, `--allow <domain>`(반복). allowlist 출처는 **정책 프로파일의
  egress**(`spec.Policy().Egress()` → `NormalizeAllowlist`) — world-config가 아니다.
- `authorize` 순서: `splitTarget` → `allowed(domain)` → `LookupIPAddr` → 모든 주소 `isPublicIP`
  → CONNECT면 port 443 강제 → allow audit → `addresses[0]`로 dial. HTTP 전달은 pinned
  resolved address로 dial(`serveHTTP` 140행).
- `egressPayload`(contracts)는 `{domain, method, size_bytes, at_ms, decision}` 폐쇄.
- world-config(`surfaces/hx/run_production.go` `worldConfig`): `state_root`, `proxy_image`,
  `adapters{bin,image,agent_argv,control_mode,env}`. `DisallowUnknownFields`. 검증은
  `parseWorldConfig`(claim **이전**, T17 P1 선례 — 잘못된 운영자 설정이 key를 소모하지 않음).
- 에이전트 컨테이너 env: `HTTP_PROXY/HTTPS_PROXY/http_proxy/https_proxy=sidecar`, `NO_PROXY=`.

## 3. 제안

### 3.1 world-config 필드 (optional, 기본 없음)
```json
"egress_pins": [
  { "domain": "<gateway-host>", "address": "<gateway-ip>:<port>" }
]
```
검증(`parseWorldConfig`, claim 이전 fail-closed):
- `domain`: `normalizeDomain` 통과(소문자 ASCII DNS 이름, **IP 리터럴 금지**), 중복 금지.
- `address`: 정확한 `ip:port`(호스트명 금지). ip는 **loopback·link-local·multicast·unspecified
  거부**; private/CGNAT는 허용(그것이 목적). port 1~65535.
- 핀 개수 상한 4(span당). 상한·형식 위반은 `UNSUPPORTED_CONTRACT`가 아니라 world config 오류
  (운영자 설정)로 거부.

### 3.2 사이드카 인자와 프록시 의미론
- `proxyCreateArgs`가 `--pin <domain>=<ip:port>`(반복)를 전달. `hxegressproxy` main이 파싱해
  `Config.Pins map[domain]netip.AddrPort`로 `New`에 넘김(`New`에서 동일 검증 재수행).
- `authorize` 변경점 — **핀 도메인일 때만**:
  1. `allowed(domain)` 통과는 **여전히 필수**(핀은 정책을 넓힐 수 없다).
  2. **DNS 해석 생략**, ip = 핀 ip. `isPublicIP` 미적용(핀 검증이 대체; 리바인딩은 DNS가
     없으므로 원천 불가).
  3. 요청 port ≠ 핀 port → deny(audit reason "pinned port 불일치").
  4. CONNECT는 기존대로 port 443만(핀 port가 443이 아니면 CONNECT는 deny — http 전달만 가능).
- 비핀 도메인 경로는 **바이트 동일**(회귀 테스트로 고정).

### 3.3 Prepare 시 교집합 검사
`Backend.Prepare`에서 핀 domain ∉ 병합 정책 allowlist이면 **거부**(claim 이전·외부 효과 이전).
핀은 정책이 허용한 도메인의 *주소 해석 방식*만 바꾼다 — allow 집합은 불변.

### 3.4 감사
`egressPayload` 무변경(폐쇄 스키마). 핀 사용은 allow 이벤트의 `domain`으로 식별되고, 핀 선언의
durable 근거는 운영자 world-config 파일이다. spawn metadata에 pins를 기록하는 것은 contracts
변경이라 **v2 후보로만 기록**, 지금은 하지 않는다.

## 4. 불변식 접촉

- FR-SBX-03 기본차단·강제 프록시: 핀 도메인 외 모든 경로 무변경. 핀 도메인도 allowlist
  교집합·port 일치·audit 전부 유지. **좁아지기만**: 핀은 world-config(운영자)에만 있고 정책
  프로파일 병합 경로에 없으므로 overlay가 핀을 추가해 넓힐 수 없다.
- DNS 리바인딩 방어: 핀 도메인은 DNS를 아예 쓰지 않아 기존보다 강함.
- FR-COL-03 audit: allow/deny 시도 기록 동일.
- T17 P1(잘못된 운영자 설정은 key 미소모): 3.1·3.3 검증이 claim 이전.

## 5. 명시적 비범위

- IP 리터럴 BASE_URL/allowlist 허용 — 하지 않음(도메인 allowlist 모델 유지).
- 와일드카드·suffix 핀 — 하지 않음(정확한 도메인 1:1).
- 핀을 정책 프로파일/overlay에 두는 것 — 금지(§4).
- 프록시 우회 플래그(`HX_SKIP_EGRESS_PROXY` 류)를 main에 들이는 것 — 하지 않음.
- `isPublicIP` 자체의 CGNAT 규칙 완화 — 하지 않음.

## 6. 완료 기준 (코드 트랙, TASKS 등록은 비준 후)

- (a) 핀 도메인 요청: Resolver 호출 **0회**, 핀 주소로 dial, allow audit 1건 — `proxy_test`.
- (b) 핀 port 불일치 deny·비핀 CGNAT 도메인 deny(기존 규칙) — `proxy_test`.
- (c) 핀 domain ∉ allowlist → `Prepare` 거부, claim 미소모 — `local_test` + `run_production_config_test`.
- (d) 불량 핀(loopback·호스트명·IP 리터럴 domain·중복·상한 초과) world-config 거부 — `run_production_config_test`.
- (e) 비핀 경로 회귀: 기존 `proxy_test`·`local_test` 무수정 green, `proxyCreateArgs` 핀 없을 때 argv 바이트 동일.
- `make ci` green + t15 게이트 무손상. **실물 증명 = deployment host 실물 검증 2건**(토큰 0): 핀 게이트웨이
  → 통과 + collector/egress allow 1건, `example.com` → 403 + deny 1건.

## 7. 배포 영향

`hxegressproxy` 이미지 재빌드 → 새 digest → world-config `proxy_image.digest` 갱신 + `egress_pins`
추가 + `hx` 재설치. `claudecode`·`worldadapter` 무변경. (Rhizome 리뷰어 세션이 deployment host 재배포 담당.)

## 8. 연관

- D-track(world-모드 tool_use 정지)·합성 API 에러 정규화·컨테이너 tool_use 관통 게이트는 **별건
  T27 후보**로 분리(본 SCP와 독립). 실물 검증 ②는 수행 완료(§1.1) — audit 경로 결함 없음 확인,
  대신 deployment host 네트워크 도달성 결함 발견. 도달성 수리 후 실물 검증 ②를 재수행해 가드 발화를
  확인한 뒤, 본 SCP 재배포 후 실물 검증 ①.
