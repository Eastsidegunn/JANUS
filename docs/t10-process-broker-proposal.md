# T10 재제안 — container process broker와 durable-start 경계

상태: **승인 대기**. 대상: FR-SBX-01, FR-SBX-05, FR-SBX-06,
FR-ADP-01, FR-ADP-10, FR-LOG-09.

이 문서는 `docs/t10-scope-determination.md`의 확정에 따라 T10을 실제 container
test subagent까지 완성하기 위한 내부 process broker를 정한다. Claude Code의
sandbox 실행과 자격증명은 T15 범위이며 여기서 구현하거나 성립을 주장하지 않는다.
contracts·fixtures 변경과 신규 외부 의존성은 제안하지 않는다.

---

## 0. 현행 공백과 실측 근거

현재 `world.Lease.AdapterEndpoint()`의 주석은 agent의
start/stdin/stdout/stop/wait broker라고 선언하지만, local backend는 approval
broker의 `intent`/`next` endpoint를 넣는다. `claudecode`는 그 endpoint를 승인에만
쓰고 native process는 host `procgroup.Start()`로 실행한다. 또한 `Backend.Open()`은
lease 반환 전에 network·proxy·agent container를 create/start한다. 따라서 현재
계약으로는 process 경계를 넘길 수도, spawn durable ACK를 시작보다 앞세울 수도 없다.

### 0.1 공식 문서에서 확인한 범위

- `podman attach`는 **running container**의 stdin/stdout/stderr에 붙는다. stdin은
  기본적으로 연결된다.
- `podman wait --condition exited`는 container 종료를 기다리고 container exit code를
  출력한다.
- `podman image inspect`의 `.Digest`는 `sha256:` + 64자 해시다.

근거:

- <https://docs.podman.io/en/latest/markdown/podman-attach.1.html>
- <https://docs.podman.io/en/latest/markdown/podman-wait.1.html>
- <https://docs.podman.io/en/latest/markdown/podman-start.1.html>
- <https://docs.podman.io/en/latest/markdown/podman-kill.1.html>
- <https://docs.podman.io/en/latest/markdown/podman-image-inspect.1.html>

문서가 말하지 않는 lifecycle 조합은 Ubuntu CI의 일회성 프로브로 확인했다.

| run | 결과 | 설계 영향 |
|---|---|---|
| `32577032377` | **실패**: local image의 digest 문자열만으로 `podman create sha256:…`하면 `image not known` | 현재 local backend의 digest 단독 실행은 실물에서 실패한다. 실행은 `repository@sha256:…`, 로그는 digest만 사용한다. |
| `32577089234` | **실패**: container exit 7인데 `podman attach`는 0 반환 | attach CLI의 종료값은 agent 상태의 근거가 아니다. |
| `32577141727` | **성공**: scratch build digest 확정, `repository@digest` 실행, start 후 interactive attach, stdout/stderr 분리, `podman wait`=7, attach=0, PID 1 종료 시 stdout 상속 자손 정리, stop 후 PID=0·process table 없음 | 단일 `podman wait`를 종료 권위로 두고 attach는 byte transport로만 쓴다. container cgroup 종료를 T7의 process-group kill 대응물로 쓴다. |
| `32577427156` | **실패**: 원자적 `start --attach --interactive` 조합은 정상 동작했으나 attach가 0일 것이라는 프로브 기대와 달리 container exit 9를 반환 | attach 형태에 따라 CLI 반환 의미가 달라지므로 상태 권위로 쓰지 않는다. |
| `32577481909` | **성공**: `start --attach --interactive`가 첫 byte부터 stdin/stdout/stderr를 보존했고 병렬 `podman wait`와 모두 exit 9를 관측 | process start와 attach를 이 조합으로 원자화하되, 독립 `podman wait`만 권위로 둔다. |

성공 run은 Podman 5.8.4/Ubuntu runner의 해당 조합만 증명한다. 아직 실제 broker,
overlay·network·approval 관통은 증명하지 않았으며 T10 Linux integration gate가
그 근거가 된다.

---

## 1. Q1 — process broker protocol

### 1.1 선택지

| 안 | 판단 | 실패 모드 |
|---|---|---|
| host adapter가 Podman CLI를 직접 실행 | 탈락 | backend 교체 시 adapter 수정, Podman 권한이 adapter로 확산, seam 간 결합 |
| Podman API socket을 adapter에 전달 | 탈락 | container 전체 제어 권한을 가진 ambient capability가 생기고 remote backend와 계약이 달라짐 |
| approval broker에 process operation 추가 | 탈락 | agent에 보이는 request-only relay와 host-only process capability의 위협 경계가 섞임 |
| **별도 host-only process endpoint + bounded framed stream** | **선택** | 별도 protocol이 필요하지만 권한과 lifecycle이 한 lease에 묶이고 backend가 교체돼도 adapter 계약은 유지됨 |

`core/world/processwire`가 wire 형식만 소유하고 local backend와 host adapter가 이를
각각 import한다. `core/world/approvalwire`와 socket·capability·message type을
공유하지 않는다. process endpoint와 approval endpoint는 sibling host socket이며
둘 다 agent container에 mount하지 않는다. container에는 기존 request-only
`/run/hx/approve.sock`만 보인다.

### 1.2 두 연결, 한 session, 재연결 없음

하나의 process endpoint에서 role이 다른 Unix stream 연결 두 개를 정확히 한 번
받는다.

1. **control connection**: start, stdin, close-stdin, stop, wait와
   `exit_observed`.
2. **output connection**: stdout/stderr frame과 `stream_end`.

두 연결은 첫 `hello{version, role, lease_id, span_id, capability}`로 인증한다.
role별 capability는 첫 성공 handshake에서 소비된다. control/output 중복 연결,
미상관 lease/span, 연결 순서·기한 위반은 broker fatal이다. agent가 endpoint를 볼 수
없으므로 capability는 sandbox 비밀이 아니라 host adapter 오배선·우발 연결을 막는
보조 수단이다.

재연결은 금지한다. 끊긴 직전 stdin frame이 container에 일부 또는 전부 전달됐는지,
stdout frame을 adapter가 받았는지 판별할 수 없으므로 resume은 exactly-once를
거짓으로 만든다. 어느 연결이든 끊기면 broker는 stop→kill→wait, approval pending
전건 deny, stream drain 시도 후 fatal로 닫는다. 새 lease만 재실행할 수 있다.

### 1.3 frame과 상태 머신

외부 라이브러리 없이 다음 length-prefixed binary frame을 쓴다.

```text
uint32_be frame_length   # 뒤따르는 header+payload, 0 및 상한 초과 거부
uint8     version        # v1
uint8     kind
uint8     stream         # control/stdout/stderr
uint8     flags
uint64_be sequence       # 방향별 1부터 단조 증가, gap/duplicate 거부
bytes      payload       # data는 raw bytes, control은 폐쇄 JSON 구조
```

- data chunk는 최대 64KiB다. §5.2의 최대 4MiB line은 여러 stdin/stdout frame으로
  나뉘며 adapter scanner가 다시 line을 구성한다.
- `io.ReadFull`로 header/payload를 읽고 모든 write는 short write loop를 돈다.
  중간 EOF, sequence gap/duplicate, 미지 kind/field, 길이 상한 위반은 fatal이다.
- control client→broker: `start`, `stdin_data`, `stdin_close`, `stop`, `wait`.
- control broker→client: 각 요청의 `ack|error`, 그리고 exit code/reason을 가진
  `exit_observed`.
- output broker→client: `stdout_data`, `stderr_data`, 마지막 `stream_end`.

상태는 `connected → started → stdin_closed? → exit_observed → stream_end`로만
전진한다. start/wait/stdin-close는 각각 one-shot이다. start 전 stdin, exit 뒤 stdin,
두 번째 start/wait, wait 없이 session 종료는 계약 위반이다. stop은 idempotent지만
첫 stop reason만 권위가 있다.

`start`는 별도 `podman start` 뒤 늦게 attach하지 않는다. broker가
`podman start --attach --interactive --sig-proxy=false <immutable CID>`를 한 streaming
subprocess로 시작해 첫 byte부터 세 fd를 소유한다. 동시에 정확히 한
`podman wait --condition exited`를 시작한다. run 32577481909가 이 조합의 stdin,
분리 stdout/stderr와 exit 9 일치를 확인했다. start-attach의 반환값은 wait 결과와
교차 진단할 뿐 상태 권위가 아니다.

`exit_observed`와 `stream_end`를 분리하므로 container 종료 관측이 output EOF나
소비자 backpressure에 종속되지 않는다. adapter의 `Done()`은 control frame에서,
`Drain()` 완료는 output `stream_end`에서 닫힌다. 최종 결과는 **둘을 모두** 본 뒤
확정하지만 취소·kill 판단은 `Done()`만으로 가능하다.

### 1.4 stdout/stderr 순서와 backpressure

Podman attach의 stdout/stderr는 별도 pipe다. 각 stream 내부 byte 순서는 그대로
보존한다. 두 reader는 각각 최대 64KiB 한 chunk만 보유하고 unbuffered merge로
보낸다. 한 serializer가 dequeue 시 전역 output sequence를 발급하므로 adapter는
**broker가 관측한** stdout/stderr 순서를 재현한다.

서로 다른 fd에 대한 agent의 원래 syscall 총순서는 kernel이 제공하지 않으므로
그보다 강한 순서를 주장하지 않는다. TTY로 합치면 byte/line 의미가 바뀌므로 쓰지
않는다. stdout만 §5.2 parser로 가고 stderr는 진단 stream으로 유지한다.

output socket write가 막히면 merge가 멈추고, 두 pipe reader가 멈추고, Podman attach
pipe가 차며, 마지막으로 container agent의 write가 블록된다. 중간에 unbounded queue나
drop이 없으므로 FR-LOG-09가 adapter→broker→container까지 전파된다. stdin은 frame
하나를 Podman attach stdin에 전부 쓴 뒤에만 ACK한다. adapter는 ACK 전 다음 stdin
frame을 보내지 않아 partial delivery와 무제한 inflight를 막는다.

resource exhaustion도 별도 예외가 아니다. frame 크기, handshake 시간, 두 연결 수,
inflight stdin 1개를 상한으로 두고 초과 시 평범한 agent error가 아니라 broker fatal로
기록한다. fatal 경로도 container stop/kill과 wait를 수행한다.

### 1.5 오류 우선순위

adapter가 최종 오류를 고를 때 다음 순서를 고정한다.

1. processwire framing/state/sequence 오류
2. stdout handler(§5.2 parser/contracts/writer) 오류
3. output scan/drain 오류
4. `podman wait`의 container exit 상태
5. attach CLI 오류

attach는 transport일 뿐 exit authority가 아니다. 프로브에서 container exit 7에도
attach가 0을 반환했으므로, attach 성공을 agent 성공으로 해석하지 않는다.

**검증할 실패 모드:** partial header/payload, output 소비 중단, stdin short write,
control/output 중 하나만 연결, 연결 drop, duplicate start/wait, oversized frame,
sequence gap, queue를 추가해 backpressure가 끊기는 변이, disconnect 뒤 재연결 시도다.

---

## 2. Q2 — T7 lifecycle 성질의 container 대응

| T7에서 지킨 성질 | process broker 대응 | 대응하지 않는 것·주의 |
|---|---|---|
| 단일 reap: `Process.Wait` 정확히 한 번 | lease마다 정확히 한 goroutine만 `podman wait --condition exited <immutable CID>`를 실행하고 exit code를 저장한다. start-attach CLI는 byte transport이며 상태 권위가 아니다. | Podman/conmon 내부 reap은 런타임 소유다. HX가 agent host PID에 `Wait`하지 않으며, 같은 container에 두 번째 `podman wait`를 만들지 않는다. |
| EOF와 독립적인 종료 관측 | control의 `exit_observed`는 단일 wait 결과 즉시 발행하고 output의 `stream_end`와 분리한다. | adapter가 output을 읽지 않아도 broker 내부 Done은 닫힌다. socket 자체가 막히면 원격 관측은 늦을 수 있으나 broker cleanup은 늦지 않는다. |
| process-group kill·고아 자손 stdout | graceful `podman stop --time N`, deadline/실패 시 immutable CID에 `podman kill`, 이후 단일 wait. cgroup이 descendant와 stdio를 함께 정리한다. | Unix PGID/PID 재사용 guard 대신 container Done 상태와 CID를 guard로 쓴다. 성공 probe는 PID 1 종료·stop 모두 descendant stdio/PID가 남지 않음을 확인했다. |
| watchCtx goroutine 누적 방지 | lease당 watcher 하나가 `parent.Done`과 container Done만 select한다. Done/Close에서 control·output listener/conn을 닫고 watcher가 반드시 종료된다. operation마다 context watcher를 만들지 않는다. | host adapter 자체의 `procgroup` lifecycle은 T7 구현을 그대로 유지한다. container lifecycle과 합치지 않는다. |

Podman CLI subprocess 자체도 각각 정확히 한 owner가 `Cmd.Wait`한다. streaming attach와
container wait runner를 기존 blocking `commandRunner.Run`으로 흉내내지 않고,
`Start/Wait/ClosePipes`를 소유하는 내부 runner로 분리한다. 단, container exit의 유일한
의미론적 권위는 `podman wait`다.

회귀는 T7 테스트를 삭제·약화하지 않고 process broker에 같은 형태로 추가한다:
exit-before-output-EOF, descendant-held output, 취소·stop, 반복 lease goroutine 기준선,
늦은 stop이 완료된 CID에 다시 신호하지 않음, pipe/socket closure의 `os.ErrClosed` 정밀
단정이다. 모든 channel wait는 2초 이하 상한을 둔다.

---

## 3. Q3 — Prepare → durable spawn → Activate/Start

### 3.1 선택지

| 안 | 판단 | 실패 모드 |
|---|---|---|
| `Open()` 유지 후 surface가 기록 | 탈락 | Open 안에서 container가 이미 시작돼 순서를 위반 |
| `Start()`에 bool `recorded` 전달 | 탈락 | 호출자가 `true`를 만들 수 있어 타입이 증거가 아님 |
| surface callback을 Open에 전달 | 탈락 | world seam이 surface lifecycle을 역으로 호출하고 오류·cleanup 소유권이 흐려짐 |
| **staged lease + unforgeable durable receipt** | **선택** | API가 늘지만 잘못된 순서를 표현하기 어렵고 cross-lease receipt도 거부 가능 |

### 3.2 계약

개념적 API는 다음과 같다. 정확한 이름은 구현 리뷰에서 다듬어도 stage와 소유권은
합치지 않는다.

```go
type Backend interface {
    Prepare(context.Context, SpawnSpec) (PreparedLease, error)
}

type PreparedLease interface {
    ID() PreparedID
    Metadata() SpawnMetadata
    UpperDir() string
    Activate(context.Context, SpawnReceipt) (ActiveLease, error)
    Abort(context.Context) error
}

type ActiveLease interface {
    ProcessEndpoint() ProcessEndpoint
    ApprovalEndpoint() ApprovalEndpoint
    Effects() <-chan EffectAttempt
    UpperDir() string
    Close(context.Context) error
}

// SpawnReceipt의 필드는 비공개이고 공개 constructor가 없다.
func CommitSpawn(context.Context, *logd.Writer, PreparedLease, SpawnRecord) (SpawnReceipt, error)
```

`Prepare`는 preflight, realpath/scope 재검사, image inspect, state/overlay directory 준비와
metadata 계산까지만 한다. **Podman network/container create/start와 broker listener
생성은 하지 않는다.** surface는 반환 즉시 `defer prepared.Abort(...)`를 건다.

`CommitSpawn`은 다음을 한 함수에서 수행한다.

1. record가 `subagent/spawn`, child span, parent span인지 검사한다.
2. payload의 backend/profile/digest/mount가 `PreparedLease.Metadata()`와 정확히 같은지
   canonical 비교한다. local prepared에 `world_backend:none`은 거부한다.
3. 유일한 `logd.Writer.Submit`을 호출하고 durable seq ACK를 받는다.
4. ACK 뒤에만 prepared ID·metadata hash·seq에 묶인 비공개 `SpawnReceipt`를 반환한다.

`Activate`는 zero/cross-lease/reused receipt를 Podman 호출 전에 거부하고 receipt를
one-shot 소비한다. 그 뒤 network·audit/approval/process broker·proxy·agent container를
create하되 agent는 `created` 상태로 둔다. host adapter가 process endpoint에 연결해
`start` frame을 보낼 때 비로소 `podman start --attach --interactive`가 실행된다.
따라서 durable spawn은 container create보다도 먼저이며 agent 실행보다 반드시 먼저다.

`ActiveLease`에서만 endpoint를 얻을 수 있고 `subagent.Spec`에는 lease나 UpperDir가
아닌 core 소유 `AgentDescriptor{ProcessEndpoint, ApprovalEndpoint, SpanID}`만 넣는다.
production surface는 already-recorded descriptor 전용 spawn 경로를 사용한다. 기존
`world_backend:none` spawn은 T7 seam/null tests에만 남고 production registry에는 없다.

### 3.3 cleanup과 실패 의미론

- prepare 실패: 생성한 state/overlay directory를 함수가 즉시 정리한다.
- 기록 실패: surface defer가 `Abort`를 호출한다. Podman create/start는 0회다.
- activate 일부 실패: 생성된 broker/container/network를 역순 정리한다. spawn 기록은
  이미 durable하므로 같은 child span에 합성 `subagent/done{status:"error"}`를 writer로
  남긴다. 이 기록마저 실패하면 writer terminal 원인을 반환하고 성공으로 숨기지 않는다.
- active close: 기존 process stop → effect drain/ACK → mount/network cleanup 순서를
  유지한다. upper는 T11 collector ACK 전 삭제하지 않는다.
- surface 취소: `context.WithoutCancel` 기반 bounded cleanup을 사용하되 대기는 모두
  deadline을 가진다.

회귀는 writer가 session/start만 성공하고 spawn에서 실패하도록 주입한 뒤
`network create`, `container create`, `container start`, adapter spawn이 모두 0회임을
단정한다. zero/cross/reused receipt도 같은 부작용 0 단정을 가진다. 반대로 ACK 성공
경로는 command 기록상 `writer ACK < create < start`를 단정한다.

---

## 4. Q4 — 실제 container test subagent

### 4.1 helper는 무엇을 하는가

`seams/world/local/testagent`에 CGO 없는 정적 Go 실행 파일을 둔다. 이름에 Fake/Null을
붙이지 않는다. 이것은 실제 OCI process·stdio·filesystem·network·approval 경계를
검증하는 integration artifact이며 production registry에는 노출하지 않는다.

helper는 stdin/stdout으로 §5.2 NDJSON을 직접 말한다.

1. task를 받기 전에는 어떤 event도 출력하지 않는다.
2. `subagent/ready`를 첫 event로 출력한다.
3. `/workspace`에서 lower 파일 수정, 새 파일 생성, 기존 파일 삭제를 실제 수행한다.
4. proxy 환경을 제거한 direct external IP 연결을 시도해 route 부재로 실패하는지
   기록한다.
5. allowlist domain의 HTTP/HTTPS CONNECT를 시도하고, 금지 domain을 시도한다.
6. `subagent/tool_call`을 먼저 출력한 뒤 `/run/hx/approve.sock`으로 exact raw hook
   요청을 보내 decision을 기다린다. allow일 때만 marker 파일을 만들고, deny일 때는
   effect가 없도록 한 뒤 각각 `tool_result`를 출력한다.
7. usage와 done을 출력하며 stop command에는 `done(stopped)`으로 종료한다.

approval relay wire는 helper가 private local struct를 복사하지 않도록 core 소유의
request-only `approvalrelaywire`로 옮긴다. process command나 approval response 주입
operation은 추가하지 않는다.

host에는 독립 실행 `worldadapter`를 둔다. core의 §5.2 task/message/stop을 process
control stdin으로 전달하고, process stdout의 §5.2 events만 core stdout으로 전달한다.
stderr는 adapter stderr로 분리한다. tool_call을 forward하기 전에 approval endpoint에
intent를 등록하고, relay가 보낸 hook을 `approval_request`로 올리며 core의
approval_response를 relay decision으로 돌린다. 이 순서는 T10-5의 durable
policy/decision-before-response를 우회하지 않는다.

### 4.2 scratch image와 immutable reference

CI는 repository source에서 다음 순서로 만든다.

1. `CGO_ENABLED=0 GOOS=linux GOARCH=$runner_arch go build -trimpath`로 helper와
   proxy helper를 각각 빌드한다.
2. 고정 `Containerfile`은 `FROM scratch`, `COPY`, numeric non-root `USER`,
   `ENTRYPOINT`만 가진다. base pull이나 package install이 없다.
3. 임시 local repository tag로 `podman build --pull=never`한다.
4. `podman image inspect --format '{{.Digest}}' repository:tag` 결과가
   `^sha256:[0-9a-f]{64}$`인지 검사한다.
5. 실행은 tag나 digest 단독이 아니라 `repository@sha256:…`로 한다. spawn event에는
   기존 schema대로 digest만 기록한다. tag는 build lookup 외에는 사용하지 않는다.

run 32577032377이 digest 단독 실행 실패를 확인했으므로 `SpawnSpec`은 trusted local
repository와 digest를 분리해 보존하는 immutable image reference로 바꾼다. metadata와
로그에 repository/tag를 추가하지 않으며 contracts 변경은 없다.

scratch에는 CA bundle이 없으므로 허용 HTTP와 CONNECT transport 자체를 검증한다.
CONNECT 뒤 TLS 인증 성공을 T10 근거로 삼지 않는다. 외부망 변동 때문에 allow target이
실패하면 audit의 allow-before-dial과 pinned dial 시도까지는 구분해 기록하되, Linux
integration의 필수 성공 target은 구현 PR에서 일회성 probe로 먼저 확정한다. private
test sidecar를 allow하면 DNS rebinding 차단을 약화하므로 그런 예외는 만들지 않는다.

### 4.3 T10 Linux integration gate

`make world-integration`은 Linux/rootless/native-overlay/Podman 조건이 하나라도 없으면
skip하지 않고 실패한다. 다음을 하나의 실제 surface run에서 단정한다.

- spawn durable seq가 process start보다 앞섬.
- helper PID가 container 안에 있고 host adapter binary/hash는 agent가 접근·변조할 수
  없음. process/approval host socket과 Podman socket도 agent에서 접근 불가.
- lower 불변, upper create/modify, subuid-owned char-device whiteout(delete)을 owner
  filter 없이 lstat로 확인.
- direct external **IP literal** 실패, forbidden domain 실패+deny audit, allow domain
  proxy 경유+allow audit. 요청 본문/header/credential은 audit에 없음.
- approval allow 뒤 marker 생성, duplicate/deny 뒤 effect 0, 로그에 allow와
  duplicate-deny 및 native rejected/done을 모두 재구성.
- output 폭주 중 writer gate를 닫아 helper가 block되고, gate 해제 뒤 frame/event가
  한 건도 유실되지 않음.
- abnormal exit·stop·orphan descendant에서 wait/stream/cleanup이 bounded 완료.

FakePodman 단위 테스트나 이번 일회성 probe는 이 gate의 대체물이 아니다.

---

## 5. Q5 — endpoint 오배선 제거

generic `Endpoint` 하나에 의미가 다른 capability를 담은 것이 이번 정지의 직접
원인이므로 이름만 고치지 않고 타입을 나눈다.

```go
type ProcessEndpoint struct { /* 비공개 network/address/capability */ }
type ApprovalEndpoint struct { /* 비공개 network/address/capability */ }

type ActiveLease interface {
    ProcessEndpoint() ProcessEndpoint
    ApprovalEndpoint() ApprovalEndpoint
    // Effects, UpperDir, Close …
}
```

- `AdapterEndpoint()`는 제거한다. deprecated alias를 남기면 오배선이 계속 컴파일되므로
  두지 않는다.
- 현재 `core/world/brokerwire`는 실제 역할에 맞게 `approvalwire`로 이름을 바꾼다.
- 새 `processwire`는 process frame만 가진다. 두 package 사이 type alias나 conversion
  helper를 만들지 않는다.
- `subagent.Spec`은 core 소유 `AgentDescriptor`만 받으며 endpoint raw field, capability,
  UpperDir를 agent task payload에 직렬화할 API가 없다.
- boundarylint는 worldtest와 마찬가지로 process/approval endpoint 구현 package의
  production import가 surface 조립 방향을 벗어나지 않는지 회귀를 추가한다. seam 간
  예외 규칙은 만들지 않는다.

회귀는 reflection/compile-time assertion으로 `ProcessEndpoint`와
`ApprovalEndpoint`가 상호 대입되지 않고, local lease의 두 getter가 서로 다른 socket을
반환하며, agent create args에는 host endpoint·capability·Podman socket·adapter path가
0건임을 단정한다.

---

## 6. 구현 순서와 중지 조건

승인 후에도 한 커밋에 섞지 않는다.

1. core staged lease/receipt와 nominal endpoint, Fake lifecycle·부작용 0 회귀.
2. processwire framing/state machine과 partial I/O·disconnect·backpressure 단위 테스트.
3. local Prepare/Activate, immutable `repository@digest`, process broker의
   attach/wait/stop/kill lifecycle.
4. 독립 실행 worldadapter + container testagent + approvalrelaywire 공용화.
5. surface 조립: metadata commit → receipt → activate → adapter spawn. production none
   거부와 FR-SBX-06 추적 행 완료.
6. Linux `world-integration`을 `ci-linux`에 넣고 실제 Podman gate green 확인.
7. 성공 뒤 `BLOCKED.md` T10-6 항목을 해소로 전환한다. T15 미충족 행은 유지한다.

다음이면 우회하지 않고 다시 정지한다.

- process connection의 backpressure가 container write까지 전파되지 않음.
- `podman wait`와 output drain을 독립적으로 소유할 수 없음.
- attach 뒤 task를 보내기 전에 output이 유실되거나 stdin이 전달되지 않음.
- agent에서 host process/approval endpoint 또는 Podman socket에 접근 가능.
- 실제 integration에서 direct IP route, overlay, approval correlation 전제가 기존
  probe와 다름.
- stdlib만으로 framed stream을 안전하게 구현할 수 없어 신규 의존성이 필요함.

---

## 7. 승인 요청

| # | 항목 | 성격 |
|---|---|---|
| 1 | Q1 별도 process endpoint, 두 role connection, framed v1, 재연결 금지, bounded backpressure·오류 우선순위 | process protocol 설계 |
| 2 | Q2 `podman wait` 단일 종료 권위와 attach transport 분리, stop→kill→wait 대응 | lifecycle 설계 |
| 3 | Q3 `Prepare → CommitSpawn receipt → Activate/START` staged lease와 concrete writer ACK 결속 | core/world 계약 변경 |
| 4 | Q4 실제 §5.2 testagent·worldadapter와 FROM scratch integration artifact | T10 범위·테스트 설계 |
| 5 | Q5 `ProcessEndpoint`/`ApprovalEndpoint` nominal 분리, `AdapterEndpoint` 제거, `brokerwire`→`approvalwire` | 오배선 제거 변경 |
| 6 | probe 발견 반영: digest 단독 실행 금지, trusted repository + digest로 `repository@digest` 실행 | local image 계약 정정 |
| 7 | contracts·fixtures 변경 없음, 신규 외부 의존성 없음, Claude Code sandbox는 T15 유지 | 범위 확인 |

승인 전에는 staged lease, process broker, surface, workflow 코드를 구현하지 않는다.
