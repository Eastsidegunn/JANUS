# T10 사전 제안 — world local 백엔드 (rootless Podman)

상태: **재승인 대기** (2026-08-20, 1차 리뷰 차단 4건 반영). 이 PR은 제안서 한 파일만 추가한다. 코드,
워크플로, `contracts/`, 픽스처, Go 모듈은 변경하지 않는다. 대상 요구사항은
FR-SBX-01~06, FR-ADP-10, FR-POL-05이며, 릴리스 게이트는 §8-4다.

실행 환경의 근거는 `t10/runtime-probe`의
`docs/t10-runtime-findings.md`(GitHub Actions run 32270359463)다. 그 조사로
ubuntu-latest의 Podman 5.8.4, 추가 설정 없는 rootless 실행, 커널 native
overlayfs, 충분한 용량을 확인했다. macOS에는 런타임을 설치하지 않고 실제 통합
검증은 Linux CI에서만 한다.

## 0. 결론

| 질문 | 선택한 안 |
|---|---|
| Q1 workspace | 사용자 관리 `upperdir`/`workdir`를 지정한 Podman `:O` overlay |
| Q2 egress | agent 전용 internal bridge + 외부망도 가진 감사 프록시 sidecar |
| Q3 approval | 실제 T9 소켓은 호스트에 유지하고, 한 번 쓰는 tool intent와 정확히 상관되는 제한 relay만 컨테이너에 노출 |
| Q4 world seam | `core/world` 계약 + `seams/world/local` 구현 + `surfaces/hx` 조립; 어댑터에는 backend-neutral broker endpoint만 전달 |
| Q5 spawn metadata | `world_backend` 판별 폐쇄 분기; local-Podman branch에만 sandbox metadata 필수 |
| Q6 tests | `world-integration` 타깃을 Linux에서 fail-closed로 실행하고 CI는 `ci-linux`로 묶음 |
| Q7 Fake | `core/world/worldtest.FakeBackend`, 테스트 파일에서만 import 가능하도록 lint 강제 |

두 가지를 보안 경계로 보지 않는다. 환경변수 `HTTP_PROXY`만으로는 강제 프록시가
되지 않으며, 컨테이너 안의 비밀 토큰만으로는 같은 UID의 에이전트와
`hxapprove`를 구별할 수 없다. 아래 선택은 이 두 전제를 회피한다.

### 0.1 문서 추론이 아니라 조합 실행으로 확인한 전제

1차 리뷰의 지시에 따라 구현 전에 별도 일회성 workflow를
`t10/world-local-probe2`에서 실행했다. 최종 성공 run은
[32352803170](https://github.com/Eastsidegunn/JANUS/actions/runs/32352803170),
검증 commit은 `d17a8a4`다. workflow는 결과 확인 뒤 `5502210`에서 제거했으며
제안 PR에는 들어오지 않는다.

| 단정 | ubuntu-latest 실측 |
|---|---|
| custom `:O,upperdir,workdir` + `--userns=keep-id` lower 불변 | pass |
| upper에 create/modify/delete 표현 | pass |
| upper create/modify 소유자 | runner UID `1001` |
| 실제 조합 실행 뒤 native overlay | `Native Overlay Diff: true` 유지 |
| internal agent의 proxy 이름 해석 | `hx-proxy-…` → `10.89.0.2`, ping 성공 |
| proxy sidecar의 외부 DNS | `example.com` A/AAAA 해석 성공 |
| internal agent의 외부 DNS | `example.com: NXDOMAIN` |

이 표의 범위를 넘겨 인용하지 않는다. 마지막 항목이 증명한 것은 **이름 해석
차단**이지 경로 차단이 아니다. 외부 IP 리터럴 직결 차단은 §6 통합 테스트 2번에서
실측한다. 또한 삭제 항목의 소유자는 runner가 아니라 subordinate UID였다(§1).

초기 probe 두 번은 각각 Alpine image에 `httpd`가 없었던 것과 BusyBox
`nslookup`이 A record를 받은 뒤 host search suffix NXDOMAIN 때문에 exit 1을 낸
probe 결함이었다. 최종 run은 이름의 실제 소비를 ping으로 단정하고 전 단계가
fail-closed로 green이다.

---

## 1. Q1 — workspace overlay (FR-SBX-01, FR-SBX-02, FR-COL-02)

### 선택지와 탈락 사유

| 안 | 판단 | 실패 모드 |
|---|---|---|
| `-v <workspace>:/workspace` bind | **탈락** | 컨테이너 쓰기가 host 원본을 즉시 변경한다. lower/upper 분리가 아니다. |
| 기본 `:O` | **탈락** | lower/upper 의미는 맞지만 Podman 관리 upper가 컨테이너 종료와 함께 제거되어 T11이 읽을 안정된 경로가 없다. |
| host에서 직접 overlay mount 후 bind | **탈락** | rootless mount 권한과 host mount 정리 책임을 별도로 만들며 Podman이 이미 제공하는 user namespace 결합을 우회한다. |
| custom `:O,upperdir=…,workdir=…` | **선택** | Podman이 lower를 원본으로 쓰고, 사용자 관리 upper/work는 종료 뒤에도 보존한다. |

Podman 공식 문서는 `:O`의 host source가 overlay lower, 별도 저장소가 upper라고
정의하고, custom `upperdir`/`workdir`는 사용자가 관리하며 컨테이너 종료 때
삭제하지 않는다고 명시한다. §0.1에서 이 옵션과 keep-id의 rootless 조합도 실제로
lower 불변, upper 세 변경 유형, runner 소유권, native overlay 유지까지 확인했다.
따라서 world local은 다음 규약으로 구성한다.

```text
lower      = policy가 허용한 원본 workspace 절대 경로
mountpoint = /workspace
state root = <HX_STATE_DIR>/world/<trace_id>/<span_id>/
upper      = <state root>/overlay/upper
work       = <state root>/overlay/work
```

- state root는 lower 바깥에 만들고 mode `0700`으로 고정한다. lower 아래에 upper를
  만들면 overlay가 자기 변경 디렉터리를 다시 lower로 보게 되므로 시작 전에
  거부한다.
- lower는 `EvalSymlinks`로 실경로를 확정한 뒤 policy의 허용 scope에 다시 포함되는지
  검사한다. `/workspace/link -> /etc` 같은 symlink가 lexical path 검사를 우회하지
  못한다. Podman `-v`의 colon/comma option grammar에 안전하게 표현할 수 없는
  경로도 임의 escaping을 추측하지 않고 fail-closed 거부하며 전용 회귀로 고정한다.
- `upper`와 `work`는 같은 파일시스템인지 `stat`의 device ID로 검사한다. 다르면
  overlay 조건을 만족하지 않으므로 Podman을 실행하지 않는다.
- mount는 `-v <lower>:/workspace:O,upperdir=<upper>,workdir=<work>` 한 곳에서만
  만든다. `:U`는 lower를 재귀 `chown`하여 원본 메타데이터를 바꾸므로 사용하지
  않는다.
- agent image가 선언한 숫자 UID/GID를 `AgentUID/AgentGID`로 확정하고
  `--userns=keep-id:uid=<AgentUID>,gid=<AgentGID>` 및 같은 `--user`로 실행한다.
  Podman의 keep-id는 호출한 host UID/GID를 지정 container UID/GID에 매핑하므로,
  생성·수정의 copy-up 결과는 host에서 rootless runner 소유로 남는다(§0.1 실측:
  uid 1001).
- **삭제는 소유자가 다르다. 이것은 이상이 아니라 정상 표현이다.** §0.1 실측에서
  삭제 항목은 `type=c uid=165536 gid=165536`, 즉 subordinate UID 소유의
  character-device whiteout으로 남았다. 판정에는 `lstat`만 필요하고(char device +
  rdev 0,0) `open`은 필요 없으며, 부모 디렉터리가 runner 소유라 조회도 정리도
  된다.
  따라서 collector는 upper 항목을 **소유자로 필터링해서는 안 된다.**
  `owner == runner`는 자연스러워 보이는 검사지만 그렇게 하면 모든 삭제가 조용히
  누락된다 — 이 규약이 막으려는 바로 그 실패다. 읽기 불가는 소유자가 아니라
  `lstat` 실패로만 판정하고, 그때는 조용히 누락하지 않고 수집 실패로 처리한다.
- `UpperDir`는 agent나 어댑터에 주지 않는 host-only lease 필드다. T11 collector가
  upper의 whiteout/copy-up을 읽고 durable 이벤트 기록을 끝냈다는 ACK를 보낸 뒤에만
  world가 state root를 정리한다. agent 종료만으로 삭제하지 않는다.
- 원본 workspace는 lease가 살아 있는 동안 HX 밖에서 변경하지 않는다는 배타
  규약을 둔다. Podman도 overlay 사용 중 source 변경은 예기치 않은 실패를 낳는다고
  경고하므로, 변경을 발견하면 결과를 정상 diff로 가장하지 않고 run을 실패시킨다.

**검증할 실패 모드:** lower 직접 변경, upper/work 다른 filesystem, upper 조기
삭제, 잘못된 UID mapping, whiteout 누락, overlay가 fuse로 강등되는 경우다. CI는
원본 hash 불변, upper의 create/modify/delete 표현, host 소유자 UID, native overlay
사전 조건을 각각 단정한다. native가 아니면 skip하지 않고 실패한다.

---

## 2. Q2 — default-deny egress와 강제 프록시 (FR-SBX-03, FR-COL-03)

### 선택지와 탈락 사유

| 안 | 판단 | 실패 모드 |
|---|---|---|
| Podman 기본 network | **탈락** | 실측상 외부 egress가 허용된다. |
| `--network=none` + proxy 환경변수 | **탈락** | proxy 자체에도 도달할 수 없고, host socket을 추가해도 proxy를 모르는 프로그램의 시도는 domain 단위로 관측되지 않는다. |
| 기본 network + `HTTP_PROXY` | **탈락** | agent가 환경변수를 지우고 직접 연결할 수 있다. 강제가 아니다. |
| host nftables/iptables | **탈락** | rootless runner가 host 방화벽을 소유하지 못하며 다른 job/서비스와 경계를 공유한다. |
| internal bridge + dual-homed proxy sidecar | **선택** | agent에는 외부 route가 없고, 외부 network를 가진 신뢰 sidecar만 유일한 application egress가 된다. |

world local은 spawn마다 rootless bridge network 두 개를 만든다.

```text
agent container ── hx-<span>-internal (--internal) ── audit proxy sidecar
                                                     └─ hx-egress (external)
```

agent는 internal network 하나에만 붙인다. Podman bridge `--internal`은 bridge의 IP
forwarding과 container default route를 없애고 외부 DNS 질의도 NXDOMAIN으로
응답한다. proxy sidecar만 internal과 external network 양쪽에 붙인다. agent에는
proxy의 internal DNS 이름으로 `HTTP_PROXY`/`HTTPS_PROXY`를 주고 `NO_PROXY`는 빈
값으로 고정하지만, 보안은 환경변수가 아니라 **직접 외부 route 부재**에서 온다.
agent가 proxy 변수를 지우면 접근이 넓어지는 대신 전부 실패한다. §0.1의 실제
dual-homed 조합에서 agent는 proxy 이름을 `10.89.0.2`로 해석했지만 외부 이름은
NXDOMAIN이었고, proxy container는 외부 A/AAAA를 정상 해석했다.

proxy는 표준 라이브러리로 만든 repo 소유 helper를 read-only OCI image에서
실행한다. 신규 Go 모듈이나 runner 설치는 없다. v0.1 지원 범위는 HTTP와 HTTPS
`CONNECT`다. UDP/QUIC, 임의 TCP tunnel, IP literal은 거부한다. allowlist는
case-fold와 trailing dot 제거 뒤 exact host 또는 label 경계 suffix로 비교한다
(`example.com`이 `evil-example.com`을 허용하지 않음). proxy가 직접 DNS를 resolve한
뒤 loopback/link-local/private/metadata 주소를 거부하므로 DNS rebinding으로 host
서비스에 닿지 못한다.

### T11에 남기는 관측 인터페이스

proxy는 **dial 전에** 다음 immutable audit record를 host world broker로 보낸다.

```text
EgressAttempt{SpanID, Domain, Method, RequestBytes, At, Decision, Reason}
```

- allow와 deny를 모두 기록한다. 본문, header 값, credential은 기록하지 않는다.
- audit channel은 proxy sidecar에만 있고 agent container에는 mount/endpoint를 주지
  않는다. 따라서 agent는 실제 시도를 만들 수는 있어도 proxy-origin record를
  위조·삭제할 수 없다.
- audit enqueue가 실패하거나 bounded queue가 차면 proxy는 요청을 허용하지 않고
  종료한다. "통신은 됐는데 기록은 없음" 경로를 만들지 않는다.
- T10은 stream과 backpressure 계약까지만 만든다. T11이 이를
  `collector/egress`로 변환해 기존 단일 writer로 기록한다. DB를 직접 여는 경로는
  없다.

직접 socket 시도는 network namespace가 차단하지만 v0.1에서 syscall/domain을
복원하지는 않는다(FR-COL-04 exec 감사는 v2). §8-4 테스트는 금지 domain의 HTTP와
CONNECT를 **유일한 통로인 proxy로** 시도하여, 연결 실패와 deny audit record가
같이 생기는 것을 단정한다. "차단됐으니 관측도 됐다"고 간주하지 않는다.

**검증할 실패 모드:** agent가 external network에 실수로 함께 연결됨, proxy
sidecar가 internal network만 가져 정상 allow도 실패함, suffix 우회, DNS private
address 우회, audit sink 장애에서 fail-open, proxy 종료 후 direct egress 가능이다.

---

## 3. Q3 — 승인 훅의 sandbox 경계 통과 (FR-ADP-10, FR-POL-05)

### 위협 모델과 선택지

host의 T9 approval Unix socket을 agent container에 그대로 bind mount하는 안은
선택하지 않는다. read-only mount라도 Unix socket의 `connect`/write를 막지 않는다.
agent와 `hxapprove`가 같은 UID·mount·환경을 보면 socket path나 컨테이너 안의
HMAC secret도 agent가 읽을 수 있으므로, "socket + secret"은 진짜 훅과 agent가
만든 가짜 요청을 구별하지 못한다.

| 안 | 판단 | 실패 모드 |
|---|---|---|
| T9 socket 직접 mount | **탈락** | agent가 arbitrary hook JSON을 보내 false approval_request와 정책 기록을 만들 수 있다. |
| container token/HMAC | **탈락** | 같은 principal인 agent가 token을 읽고 동일 MAC을 만든다. |
| socket mode/별도 UID만 사용 | **탈락** | hook을 실행할 권한을 agent가 가진 이상 helper를 임의 입력으로 호출할 수 있다. setuid helper도 호출자 의도를 증명하지 않는다. |
| host correlation relay | **선택** | source 신뢰 대신 host가 이미 관측한 실제 tool intent와 one-shot 정확 일치를 권한의 전제로 삼는다. |

### 선택한 경계

실제 T9 approval coordinator와 adapter process, adapter binary, Podman socket은 전부
host에 남고 container에 mount하지 않는다. container에는 `/run/hx/approve.sock`
하나만 보이지만 이것은 T9 socket이 아니라 spawn 전용 **제한 relay**다. relay의
프로토콜은 hook request 한 종류만 받고 adapter command나 approval response를
주입하는 명령은 제공하지 않는다.

host adapter는 native stream에서 `tool_use_id`, tool name, canonical args를 관측할
때 pending intent ledger에 등록한다. relay는 hook 원본을 다음 조건에서만 T9
coordinator에 넘긴다.

1. spawn/span이 현재 lease와 일치한다.
2. `(call_id, name, canonical args)`가 adapter가 먼저 관측한 pending intent와
   byte-equivalent 의미로 일치한다.
3. call_id가 아직 소비되지 않은 one-shot이다.
4. line 크기, pending 수, 요청률 한도를 넘지 않는다.

hook 연결이 intent보다 먼저 도착하면 relay는 bounded pending 상태로만 보관하고
**allow하지 않는다**. 동일 intent가 native stream에 나타나야 진행한다. native
stream이 이 상관 순서를 제공하지 않아 양쪽이 교착되는 것이 Linux integration에서
확인되면, matching을 생략하거나 secret으로 우회하지 않고 T10을 정지해 approval
transport를 재제안한다. T9 사람 smoke의 실제 순서는
`ready → tool_call → approval_request`여서 현재 전제와 일치하지만, integration
회귀를 계속 둔다.

pending deadline은 `min(parent ctx deadline, 남은 time budget, 600초)`다. 명시적
deadline이 없는 경우에도 600초를 상한으로 사용한다. intent 미출현, timeout,
lease 종료는 모두 durable deny를 기록한 뒤 해당 hook과 나머지 pending을 deny하고
정리한다. lease 만료까지 무기한 매달리는 상태는 없다.

agent는 tool call과 args를 알고 있으므로 relay 요청 자체를 흉내낼 수 있다. 이
설계는 그 사실을 숨기지 않는다. 그러나 일치하는 요청을 먼저 소비하면 실제
`hxapprove` 연결은 duplicate로 deny되어 tool은 실행되지 않는다. 일치하지 않는
요청은 정책 레이어에 도달하지 않는다. 따라서 가능한 영향은 **자기 run의
denial-of-service**이고, 존재하지 않는 tool/다른 args에 대한 allow나 approval
decision 위조는 아니다. decision은 여전히 T9 core가 내리고
`policy/decision`을 durable 기록한 뒤에만 반환한다.

relay는 duplicate를 socket 단계에서 조용히 버리지 않는다. 모든 syntactically
valid matching/duplicate 시도는 exact hook raw를 가진 `subagent/approval_request`와
그 뒤 `policy/decision`으로 남긴다. duplicate는 같은 `call_id`, 새 `request_id`,
`reason:"duplicate tool intent"`로 강제 deny하며 Decider를 다시 호출하지 않는다.
따라서 위조 요청이 먼저 one-shot을 소비한 race의 로그는 최소한 다음을 재구성한다.

```text
approval_request(call_id=C) → policy/decision allow
approval_request(call_id=C, reason=duplicate) → policy/decision deny(duplicate)
→ native tool_result rejected 또는 done(error), effect 없음
```

allow 한 건만 남겨 실제 실행처럼 보이는 상태를 허용하지 않는다. duplicate deny
기록 실패 시에도 deny 응답과 lease 오류 종료가 우선이며 실행을 허용하지 않는다.

adapter 변조 방지는 별도 조건으로 고정한다.

- agent rootfs는 read-only, agent는 non-root, capabilities는 all-drop이며 host PID
  namespace와 Podman socket을 공유하지 않는다.
- host adapter executable/working directory/approval socket은 어떤 mount에도 넣지
  않는다. container image의 `hxapprove`는 read-only image layer의 client일 뿐이다.
- relay는 request-only이고 malformed/flood 시 lease 전체를 fail-closed 종료한다.
- integration test는 container에서 adapter PID/path/socket/Podman socket 접근과
  adapter sentinel 수정이 모두 실패하고, 종료 뒤 host adapter hash가 같음을
  확인한다. socket endpoint가 있다는 사실만으로 "adapter 접근 가능" 판정을
  통과시키지 않는다.

이 경계는 악성 agent가 sandbox 안에서 자체 subprocess를 실행하는 것 자체를
FR-POL-05 승인으로 막는다고 주장하지 않는다. 승인 훅은 Claude tool 실행을
게이트하고, 실제 효과의 보안 경계는 Q1/Q2의 filesystem/network sandbox다.

---

## 4. Q4 — 하나의 world seam과 조립 (FR-SBX-05, FR-ADP-10)

### 선택지

| 안 | 판단 | 실패 모드 |
|---|---|---|
| `seams/subagent`가 Podman 직접 import/실행 | **탈락** | sandbox/fs가 adapter seam에 결합되고 remote backend 교체 시 소비자를 수정한다. |
| `seams/world`가 `seams/subagent`를 import | **탈락** | seam 간 수평 import이며 boundarylint가 거부한다. |
| fs/process/sandbox 인터페이스 세 개 | **탈락** | 서로 다른 lifecycle로 upper 조기 삭제, process 종료, collector drain의 원자적 순서를 잃는다. |
| core 계약 + world broker + surface 조립 | **선택** | 두 seam은 core만 보고 surface가 구현을 결합한다. |

계약은 `core/world`가 소유하고 local 구현은 `seams/world/local`에 둔다. 대략적인
모양은 다음과 같다. 이름은 구현 리뷰에서 다듬을 수 있지만 lifecycle 경계는
쪼개지 않는다.

```go
type Backend interface {
    Open(context.Context, SpawnSpec) (Lease, error)
}

type Lease interface {
    AdapterEndpoint() Endpoint // host adapter가 agent stdio/lifecycle을 쓰는 broker
    Metadata() SpawnMetadata   // profile, mount target, image digest, backend
    UpperDir() string          // host-only; agent/adapter payload에 넣지 않음
    Effects() <-chan EffectAttempt
    Close(context.Context) error // process stop → effect drain/ACK → mount/network cleanup
}
```

`SpawnSpec`은 이미 병합이 끝난 `policy.SandboxConfig`, image **digest**, workspace,
agent argv, depth/budget, span을 받는다. world가 정책을 다시 넓히거나 병합하지
않는다. `Lease` 하나가 filesystem overlay, agent subprocess, sandbox network,
effect stream과 정리 순서를 함께 소유한다.

FR-SBX-04의 credential도 이 lease lifecycle에 묶는다. `SpawnSpec`에는 값이 아니라
scope·expiry가 검증된 단기 credential handle만 들어오고, world broker가 spawn
직전에 `/run/hx/secrets` tmpfs에 mode `0400` 파일로 주입한다. host 환경 전체를
상속하거나 credential을 argv/workspace/upper에 넣지 않으며, expiry가 없거나 run
deadline보다 긴 장기 credential은 시작 전에 거부한다. lease 종료 시 tmpfs와
handle을 폐기하고 로그에는 credential 식별자·scope·expiry만 남긴다. 원문 값은
spawn metadata나 effect record에 들어가지 않는다.

독립 실행 adapter에 Go interface를 넘길 수 없으므로 local backend는 host에
per-spawn world broker endpoint를 만든다. adapter의 공용 native-process client는
그 endpoint로 start/stdin/stdout/stop/wait만 말한다. local broker는 Podman을,
향후 remote backend의 host facade는 microVM을 구동하지만 adapter와
`seams/subagent`의 소비 코드는 바뀌지 않는다. endpoint는 opaque lease capability며
agent container에는 보이지 않는다. Q3 approval relay는 world lease가 가진 별도
request-only endpoint다. process broker와 같은 socket으로 합치지 않는다.

조립 방향은 다음 한 방향뿐이다.

```text
core/world  ←  seams/world/local
     ↑                 ↑
seams/subagent         │
     ↑                 │
     └──── surfaces/hx ┘
```

`surfaces/hx`가 Backend를 선택하고 lease를 만든 뒤 core 소유 descriptor만
`subagent.Spec`에 넣는다. `seams/subagent`와 `seams/world`는 서로 import하지
않는다. boundarylint에는 `seams/world` 허용 top-level과 기존 cross-seam 금지를
그대로 적용하며, 예외 규칙을 만들지 않는다.

**검증할 실패 모드:** adapter가 Podman을 직접 실행하는 우회, lease Close 전에
collector drain 누락, endpoint가 agent에 노출됨, remote backend 선택 때문에
adapter/surface 소비 API가 분기됨, world가 DB를 직접 여는 경우다.

---

## 5. Q5 — `subagent/spawn` metadata (FR-SBX-06)

현재 `contracts/events.schema.json`의 `subagent/spawn` 분기는 kind만 고정한다.
그대로 두면 profile ID, mount path, image digest가 빠지거나 오타여도 writer의
저장 직전 검증을 통과한다. 이는 FR-SBX-06의 MUST를 런타임 관례로 낮추고 replay
근거를 잃으므로 **무제약 유지안을 거부**한다.

다만 이 PR에서는 `contracts/`를 수정하지 않는다. 다음 변경을 **SCP-T10-001**로
승인 요청한다.

### SCP-T10-001 — world backend 판별 spawn payload 폐쇄

- 위치: §5.1 `subagent/spawn` payload 설명과
  `contracts/events.schema.json`의 해당 oneOf 분기.
- 문제 1: FR-SBX-06 필수 metadata를 schema가 전혀 강제하지 않는다.
- 문제 2: 모든 spawn에 local-Podman metadata를 필수로 만들면 현재 T7 null 경로와
  비샌드박스 기록을 표현할 수 없다. writer는 저장 직전 모든 record를 검증하므로
  emitter와 schema를 함께 바꾸지 않으면 기존 관통 경로가 즉시 깨진다.
- 기존 schema annotation은 "남은 열린 kind(session/*, assistant/chunk,
  subagent/spawn)의 필드 확정은 해당 태스크에서 **additive**로 진행"한다고
  적었다. 아래 폐쇄는 non-additive이므로 이 문구를 그대로 두지 않는다.
- 단일 제안: `world_backend`를 공통 required 판별자로 두고
  `subagentSpawnPayload`를 두 폐쇄 분기로 만든다. T9 `tool_result`와 같이 각
  branch가 허용 property 전체를 반복하고 `additionalProperties:false`를 가진다.

```text
base required: [adapter, instruction, depth, budget, world_backend]

oneOf:
  - world_backend: const "none"
    properties: {adapter, instruction, depth, budget, world_backend}
    additionalProperties: false

  - world_backend: const "local-podman"
    properties: {adapter, instruction, depth, budget, world_backend,
                 profile_id, image_digest, mounts}
    required: [profile_id, image_digest, mounts]
    additionalProperties: false

image_digest: "sha256:" + lower hex 64자 (tag만 기록하는 것은 불가)
mounts: [{source_path, target_path, mode:"overlay", upper_ref}]
```

`none`은 기존 null adapter·테스트·과거 비샌드박스 경로를 명시적으로 표현한다.
schema가 `none`을 허용하는 것이 production 권한은 아니다. T10 이후 production
surface는 FR-SBX-01에 따라 `local-podman`만 선택하고 `none`을 거부한다. 향후
`remote-microvm`은 같은 판별 구조에 새 폐쇄 branch를 추가하며 consumer API는
바꾸지 않는다.

`source_path`는 정책 평가 뒤 정규화된 host workspace, `target_path`는
`/workspace`, `upper_ref`는 session state root에 상대적인 stable artifact ID다.
절대 upper path는 host 임시 경로와 사용자명을 로그에 고정하므로 payload에 넣지
않고 lease 내부 mapping으로만 보유한다. 자격증명 값, proxy token, socket path도
기록하지 않는다.

- 구현 영향(승인 뒤 한 원자적 커밋): 명세와 events schema 변경 → 모든 기존
  emitter에 `world_backend:"none"` 추가 → `make codegen` → validate 유효/위반
  회귀 → T2 generator와 기존 spawn sample 갱신 → drift gate. `subagent/spawn`은
  adapter→core wire kind가 아니므로 wire mirror를 만들지 않는다. 생성 타입은
  손으로 수정하지 않는다.
- schema의 기존 annotation은 session/*와 assistant/chunk만 향후 additive 확정
  대상으로 남기고, `subagent/spawn`은 "T10에서 world_backend 판별 branch로
  non-additive 폐쇄(승인일 기록)"라고 개정한다. annotation과 실제 계약을 같은
  커밋에서 바꾼다.
- `contracts/validate/validate_test.go`의 "어댑터가 못 내는 kind"는 payload가 새
  `none` branch에 유효한 sample로 갱신한다. 같은 payload의 full EventRecord가
  `ValidateRecord`를 통과하고 wire `ValidateEvent`만 kind 때문에 실패함을 함께
  단정해, payload 폐쇄라는 다른 이유로 우연히 green이 되는 것을 막는다.
- 변경 성격: 기존 열린 payload를 폐쇄하고 판별자를 필수화하는
  **non-additive 계약 강화**다. 명세 소유자의 [H] 승인 전 구현하지 않는다.
- 실패 모드: tag만 기록해 image가 바뀜, host 임시 upper 절대 경로 누출,
  metadata 기록 전에 container 시작, schema 실패를 무시하고 spawn 계속이다.

spawn event의 durable ACK가 성공한 뒤에만 world lease가 agent process를 시작한다.
기록 실패 시 container를 시작하지 않는다. 이 순서가 "실행됐지만 환경 근거가
로그에 없음"을 없앤다.

---

## 6. Q6 — Linux integration test 배치 (FR-SBX-01~03, FR-ADP-10, §8-4)

macOS에서 `_linux_test.go`가 조용히 제외되는 것을 성공으로 취급하지 않는다.
다음 두 타깃을 추가한다.

```make
world-integration:
	# uname != Linux, podman 없음, rootless=false, native overlay=false면 exit 1
	go test -tags='worldintegration' -count=1 -timeout=8m ./seams/world/local/...

ci-linux: ci world-integration
```

- test file은 `//go:build linux && worldintegration`으로 격리한다.
- `world-integration` recipe 자체가 `uname -s == Linux`를 먼저 검사하므로 macOS에서
  실행해도 "테스트 파일 없음"으로 green이 되지 않고 명시적으로 실패한다.
- `.github/workflows/ci.yml`의 ubuntu job은 `make ci` 대신 `make ci-linux`를
  실행한다. 기존 `make smoke`는 CGO 없는 순수 Go 경로라는 의미를 유지한다.
- workflow는 Podman을 설치하지 않는다. 사전 설치 5.8.4를 사용하고 `podman info`로
  rootless와 native overlay를 preflight한다. 조건 불충족은 skip이 아니라 실패다.
- 컨테이너 image는 registry pull에 기대지 않고 CI가 정적 test helper를 빌드해
  `FROM scratch` OCI image로 만든다. digest를 inspect해 backend에 넘기므로 tag
  drift와 registry 장애가 acceptance 결과를 흐리지 않는다.
- integration이 추가되므로 job `timeout-minutes`는 10에서 20으로 늘리되,
  개별 Go test timeout 8분과 subprocess context deadline을 둔다.
- `make lint`는 Linux/tag graph를 vet해 build-tag 코드가 테스트 실행 전부터
  부패하지 않게 한다(`CGO_ENABLED=0 GOOS=linux go vet
  -tags=worldintegration ./seams/world/local/...`). boundarylint의 linux sweep이
  world package를 함께 검사한다.

CI integration은 최소 다음을 실제 Podman process로 확인한다.

1. workspace create/modify/delete 뒤 lower hash 불변, upper diff와 owner 일치.
2. agent의 direct external 접속 실패. **DNS 차단만으로 판정하지 않는다** —
   이름 해석을 거치지 않는 외부 **IP 리터럴 직결**(TCP)이 route 부재로 실패하는
   것을 별도로 단정한다. §0.1이 증명한 것은 NXDOMAIN까지이며 경로 차단은 아직
   실측되지 않았다. 이어서 allowlisted HTTP/CONNECT만 proxy로 성공.
3. 금지 domain 연결 실패 **및 같은 시도의 deny audit record 존재** (§8-4).
4. agent에서 host adapter process/path/approval socket/Podman socket 접근 불가,
   수정 시도 뒤 adapter hash 불변 (FR-ADP-10).
5. Q3 unmatched/duplicate/mismatched relay request는 deny, matching one-shot만 core
   decision으로 진행한다. forged race 뒤 로그에서 첫 allow와 같은 call_id의
   duplicate-deny가 **둘 다** 재구성되고 native rejected/done과 effect 없음까지
   단정한다. allow만 남으면 테스트 실패다.
6. stop/crash에서도 proxy audit drain과 upper 보존이 먼저이고 cleanup은 마지막.

단위 테스트 green이나 FakeBackend 결과를 위 통합 게이트의 대체물로 인정하지
않는다. 이 여섯 검사가 원격 CI에서 green이어야 T10 완료다.

---

## 7. Q7 — local 단위 테스트용 FakeBackend

공용 계약의 테스트 지원은 `core/world/worldtest`에 둔다. 이름은
`FakeBackend`, `FakeLease`, `FakeProcess`처럼 전부 `Fake` 접두사를 붙이고 package
doc에 "테스트 전용이며 OCI/overlay/network 격리를 제공하지 않는다"고 명시한다.

Fake는 호출 기록, 명시적 event stream, injected start/write/wait/cleanup 오류,
수동 lifecycle gate만 제공한다. shell command를 실행하거나 임시 디렉터리를
overlay인 것처럼 꾸미지 않는다. production surface에는 생성자나 backend 선택
문자열을 노출하지 않는다.

`worldtest`는 `_test.go`에서만 import할 수 있도록 boundarylint에 source-file
검사를 추가한다. production `.go`의 import는 위반이다. 이렇게 해야 다른 seam의
테스트가 같은 Fake를 재사용하면서도 `seams/world`를 수평 import하지 않고,
목업이 실제 backend로 조용히 승격되는 경로도 막는다.

실제 local backend 테스트는 둘로 나눈다.

- 일반 `go test -race`: Fake를 통한 lifecycle/오류/ordering 단위 테스트.
- `make world-integration`: Podman/overlay/internal network/proxy/adapter 격리의 실제
  의미론. Fake assertion으로 대체 금지.

**검증할 실패 모드:** production import, surface backend registry에 Fake 추가,
Fake test를 FR-SBX acceptance 근거로 인용, Fake가 실제 subprocess를 실행하여 단위
테스트가 host 환경에 의존하는 경우다.

---

## 8. 구현 순서와 정지 조건

승인 뒤에도 한 커밋에 섞지 않는다.

1. **[H] SCP-T10-001 승인** → spec/schema annotation/codegen/모든 emitter/
   validate/generator를 한 원자적 커밋으로 변경.
2. `core/world` 계약 + `worldtest.Fake*` + boundarylint production-import gate.
3. world local의 rootless Podman lifecycle과 Q1 overlay.
4. Q2 proxy sidecar와 audit stream(아직 collector 이벤트로 쓰지 않음).
5. Q3 correlation relay + T9 adapter의 world broker client 조립.
6. surface wiring과 spawn metadata 선기록.
7. Linux integration target/workflow → 원격 CI green 확인.

다음 경우 우회하지 않고 `BLOCKED.md`에 기록하고 멈춘다.

- custom upper/work가 성공 probe run 32352803170과 달리 rootless native overlay에서
  성립하지 않음.
- internal bridge의 agent가 proxy를 우회해 외부에 도달함.
- native stream과 PreToolUse hook 순서상 host intent correlation이 성립하지 않음.
- adapter를 container에 넣지 않고는 backend-neutral broker를 구성할 수 없음.
- stdlib proxy로 요구한 HTTP/CONNECT와 audit-before-dial을 구현할 수 없어 신규
  의존성이 필요함.

T11의 fsdiff/collector event 생성, T12 감사, T13 보강을 선제 구현하지 않는다.
T10은 upper/effect stream의 안전한 인터페이스까지만 남긴다.

## 9. 승인 요청

| # | 요청 | 성격 |
|---|---|---|
| 1 | Q1 custom `:O` + keep-id + collector ACK 뒤 cleanup | 설계 결정 |
| 2 | Q2 internal-only agent + dual-homed stdlib proxy sidecar | 보안/설계 결정 |
| 3 | Q3 direct socket 금지 + host intent-correlated relay, residual DoS 허용 | 보안 경계 결정 |
| 4 | Q4 `core/world` 계약 + world broker + surface 조립 | 구조 결정 |
| 5 | **SCP-T10-001** world backend 판별 폐쇄 + local branch metadata 필수 | **명세/contracts [H]** |
| 6 | Q6 `ci-linux`/`world-integration`, ubuntu 전용 fail-closed gate | CI 결정 |
| 7 | Q7 `core/world/worldtest.Fake*`와 production import 금지 | 테스트 경계 결정 |

## 10. 근거

- 기능 명세: `docs/hx-기능명세서-v0.1.md` FR-SBX-01~06,
  FR-ADP-10, FR-POL-05, FR-COL-02~03, §8-4.
- 실행 환경: `t10/runtime-probe`의 `docs/t10-runtime-findings.md`, GitHub Actions
  run 32270359463 (2026-08-20).
- Q1/Q2 조합 probe: branch `t10/world-local-probe2`, commit `d17a8a4`, GitHub
  Actions run 32352803170 (2026-08-20, workflow는 `5502210`에서 제거).
- Podman `:O`, custom upper/work, keep-id:
  <https://docs.podman.io/en/latest/markdown/podman-run.1.html>
- Podman bridge `--internal`:
  <https://docs.podman.io/en/latest/markdown/podman-network-create.1.html>
- T9 approval 계약: `docs/t9-adapter-contract-proposal.md`,
  `seams/subagent/approval.go`, `seams/subagent/claudecode/hxapprove/`.
