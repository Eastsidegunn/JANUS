# T11 collector 제안 — fsdiff + egress

상태: **승인 대기**. 이 문서는 FR-COL-01/02/03/05/06과 수용 기준 §8-3/4를
구현하기 전 계약을 확정한다. 이 PR은 제안서만 추가하며 `contracts/`, fixtures,
Go 코드와 워크플로는 변경하지 않는다.

## 0. 결론

| 질문 | 선택한 안 |
|---|---|
| Q1 egress 의미 | 프록시가 관측한 allow·deny **시도 모두**를 기록하고, `decision` 판별 폐쇄 분기로 계약을 강화한다(SCP-T11-001). |
| Q2 fs diff | process 시작 전 lower 전체 기준선을 만들고, 종료 뒤 lower 불변을 재검증한 다음 upper를 owner filter 없이 결정적으로 순회한다. |
| Q3 협조 무관 수집 | baseline은 `Activate` 전, egress drain은 adapter 시작 전, fs finalize는 모든 done/error/stop/cancel 경로의 host-side defer에서 실행한다. |
| Q4 ACK와 정리 | egress `EffectReceipt`와 fs `CollectionReceipt`를 명목 분리한다. durable fs ACK 뒤 같은 lease만 upper를 정리하며 raw-path cleanup API는 만들지 않는다. |
| Q5 경계 | collector는 stdlib+contracts만 사용해 record 후보를 만든다. surface가 collector와 core writer를 조립하므로 어느 방향의 import도 생기지 않는다. |
| Q6 통합 검증 | T10의 실제 FROM-scratch testagent가 보고 없이 파일을 바꾸고, 같은 Linux gate에서 durable collector event와 upper cleanup을 확인한다. |

이 설계는 T10의 `Lease.UpperDir()`와 `Lease.Effects()`를 host-only source로
사용한다. agent·adapter payload에 upper 절대 경로나 audit endpoint를 추가하지
않는다.

---

## 1. Q1 — `collector/egress`의 allow/deny 의미 (FR-COL-03, §8-4)

### 1.1 현재 계약으로는 재구성이 불가능하다

현재 `egressPayload`는 `domain`, `method`, `size_bytes`, `at_ms`만 가진다. 같은
payload가 실제로 proxy를 통과해 외부 dial까지 간 allow인지, policy·형식·주소
검사에서 거부된 deny인지 로그만으로 구분할 수 없다. T10의 host audit record는
이미 `Decision`과 deny `Reason`을 보유하므로 이 정보를 버리는 것은 “모든 파생
상태는 이벤트 로그에서 재계산 가능” 불변식을 위반한다.

FR-COL-03의 “프록시를 통과한 요청”은 **외부로 전달된 요청만**이 아니라
**강제 프록시가 관측하고 판정한 요청 시도**로 해석한다. 그 이유는 §8-4가
allowlist 밖 시도의 차단과 `collector/egress` 기록을 동시에 MUST로 요구하기
때문이다. 외부 dial까지 성공한 요청만 뜻하면 deny 시도를 기록하라는 수용 기준과
모순된다.

### 1.2 선택지

| 안 | 판단 | 실패 모드 |
|---|---|---|
| 현행 payload 유지 | 탈락 | deny와 allow가 동일한 사실로 재생된다. |
| `reason` 유무로 decision 추론 | 탈락 | 암묵 판별이고 allow에 진단이 붙는 순간 의미가 바뀐다. schema가 상태를 강제하지 못한다. |
| allow/deny를 별도 kind로 분리 | 탈락 | 이미 확정된 `collector/egress` 어휘를 불필요하게 늘리고 소비자가 두 kind를 합쳐야 한다. |
| `decision` 판별 oneOf | **선택** | 기존 이벤트와 비호환이지만 pre-v0.1에서 명시적으로 닫고 codegen·emitter를 같은 커밋에서 바꿀 수 있다. |

### 1.3 SCP-T11-001 — egress 판정 분기 폐쇄

명세 소유자의 [H] 승인을 요청한다. 승인 전에는 `contracts/`를 수정하지 않는다.

- 명세 §5.1의 `collector/egress` payload와 FR-COL-03을 “proxy가 관측한
  allow/deny 요청 시도”로 명확히 한다.
- `contracts/events.schema.json`의 `egressPayload`를 `decision` 판별 폐쇄
  분기로 바꾼다. T9 `toolResultPayload`와 T10 `subagentSpawnPayload`의 선례처럼
  각 branch가 허용 property 전체를 반복하고 `additionalProperties:false`를
  가진다.

```text
base required: [domain, method, size_bytes, at_ms, decision]

oneOf:
  - decision: const "allow"
    properties: {domain, method, size_bytes, at_ms, decision}
    additionalProperties: false

  - decision: const "deny"
    properties: {domain, method, size_bytes, at_ms, decision, reason}
    required: [reason]
    reason: non-empty, maxLength 512
    additionalProperties: false
```

`reason`은 policy/validation 진단만 기록하고 header, body, credential, resolved IP를
넣지 않는다. IP literal 자체는 `domain` 자리에 시도 대상 문자열로 남길 수 있지만
항상 deny여야 한다. domain을 얻기 전에 형식이 깨진 요청은 빈 domain을 허용하는
현행 숫자·문자열 범위를 유지하고 non-empty reason으로 원인을 남긴다. 이 SCP는
decision 의미를 추가하는 데 한정하며 domain/method의 기존 범위를 임의로 넓히지
않는다.

승인 뒤 한 원자적 커밋에서 다음을 함께 바꾼다.

1. 명세 FR-COL-03·§5.1·§8-4 문구.
2. events schema와 `make codegen` 생성물.
3. validate 유효 allow/deny 및 분기 혼입 회귀.
4. T2 generator, replay sample, 기존 collector event sample.
5. T10 `EffectAttempt` → collector emitter.

회귀는 allow+reason, deny reason 누락, 미지 decision, decision 없는 기존 모양,
deny branch에 추가 field가 섞인 경우를 모두 거부한다. 생성 타입은 손으로 고치지
않는다.

**실패 모드:** decision 유실, deny reason에 secret 포함, allow event가 durable
commit되기 전에 실제 dial을 허용, audit queue 포화가 평범한 deny로 위장되는
경우다. 마지막 두 항목은 T10의 audit-before-dial·503 구분 회귀를 유지하고 T11의
writer failure 회귀를 추가한다.

---

## 2. Q2 — upper 기반 fs diff (FR-COL-02)

### 2.1 spawn 기준선과 lower 배타 규약

deleted hash를 종료 시점의 lower에서 처음 계산하지 않는다. 그렇게 하면 HX 밖의
프로세스가 run 도중 lower를 바꿨을 때 변경된 내용을 spawn 기준선으로 오인한다.

선택한 순서는 다음과 같다.

```text
world.Prepare
→ collector가 resolved lower 전체 baseline manifest 작성
→ subagent/spawn durable CommitSpawn
→ world.Activate
```

baseline 실패·상한 초과·경로 경합이면 spawn event와 container start 모두 0이다.
baseline manifest는 relative POSIX path, node kind, semantic SHA-256, 크기와 안전한
변경 검사용 metadata를 가진 host-only 메모리 상태다. agent/adapter에는 전달하지
않고 로그에도 원본 파일 목록을 선기록하지 않는다.

baseline의 directory는 재귀하지만 event 후보가 아니고, regular file과 symlink만
semantic hash 대상이다. socket/FIFO/device가 lower에 있으면 나중 삭제 hash를
현행 schema로 정직하게 표현할 수 없으므로 process 시작 전에 실패한다. 읽기 권한
부족도 빈 hash나 누락으로 바꾸지 않는다.

agent process와 overlay가 완전히 quiesce된 뒤 lower를 다시 전수 검사해 baseline과
비교한다. 하나라도 다르면 HX 밖 변경으로 간주해 정상 diff를 만들지 않고 run을
실패시킨다. 부분 `collector/fs_changed`를 기록하거나 upper를 지우지 않는다. 이는
T10 §1의 “lease 동안 원본 workspace 배타” 규약을 실제 검사로 만든다.

regular file은 열기 전후 `lstat/fstat` 정합을 확인하고 symlink를 따라가지 않는다.
경로 요소가 scan 중 바뀌거나 파일이 hash 중 변경되면 재시도로 숨기지 않고
collection failure다. 외부 프로세스가 lower를 바꾸는 것을 collector가 잠금으로
억지 소유한다고 주장하지 않는다.

### 2.2 upper 순회 규칙

upper는 host-only 경로에서 `lstat`으로 순회한다. UID/GID는 분류 조건에 사용하지
않는다. T10 실측에서 정상 삭제 whiteout은 subordinate UID `165536`의 character
device이며 `rdev=0:0`였다. owner filter는 모든 삭제를 조용히 누락하므로 금지한다.

모든 결과는 slash 기준 relative path로 정규화하고 bytewise 정렬한다. 같은 path에
두 의미가 생기거나 path가 lower/upper root 밖으로 탈출하면 fail-closed다.

| upper 표현 | `changes[]` 표현 |
|---|---|
| lower에 없는 regular file | `added`, 최종 upper content SHA-256 |
| lower에 있는 copy-up regular file | `modified`, 최종 upper content SHA-256. 내용 hash가 같아도 copy-up 자체가 metadata 변경일 수 있으므로 생략하지 않는다. |
| char device + `rdev=0:0` whiteout | `deleted`, spawn baseline의 lower semantic hash |
| rename | old path의 `deleted` + new path의 `added`. 현행 3종 어휘에 rename을 거짓으로 숨기지 않는다. |
| hardlink | inode로 path를 dedupe하지 않고 각 변경 path를 독립 항목으로 기록한다. 동일 내용이면 hash가 같을 수 있다. |
| symlink | 절대 따라가지 않는다. `sha256("symlink\\x00" || raw link target)`을 semantic hash로 사용해 regular content와 domain-separate한다. |
| directory | directory 자체는 “변경 파일” 항목을 만들지 않고 leaf만 재귀 처리한다. |
| opaque directory | baseline의 해당 lower subtree에서 upper에 남지 않은 leaf를 각각 `deleted`로 전개하고 upper leaf는 일반 규칙으로 처리한다. marker 자체는 path로 방출하지 않는다. |
| socket/FIFO/device(whiteout 제외) | 현행 payload가 안전한 node 의미를 표현하지 못하므로 조용히 생략하지 않고 collection failure. upper와 증거를 보존한다. |

deleted 대상이 baseline에 없거나 whiteout과 실재 upper entry가 충돌하면 손상으로
거부한다. 삭제 hash는 종료 시 lower를 다시 읽은 값이 아니라 baseline manifest의
값이다.

### 2.3 opaque·rename·symlink·hardlink 선행 probe

whiteout의 `lstat(mode+rdev)`는 T10 Linux gate `32640667260`에서 실증됐다. 반면
rootless native overlay가 opaque directory를 host upper에 어떤 xattr 이름으로
노출하는지와 rename/hardlink/symlink의 정확한 upper 조합은 아직 실행 증명이 없다.
문서 추론을 구현 근거로 쓰지 않는다.

SCP 승인 뒤 **코드 작성 전에** 일회성 Ubuntu/rootless/native-overlay probe로
다음을 만든다.

- lower subtree를 통째로 제거해 opaque directory 생성.
- regular file rename.
- symlink 추가·수정·삭제.
- hardlink 추가와 한 link를 통한 내용 수정.
- host에서 owner, mode, rdev, inode, xattr 이름/값을 raw로 출력.

probe run ID와 실패한 시도를 구현 PR에 남긴다. opaque marker를 stdlib만으로
안전하게 읽을 수 없거나 위 표와 다른 표현이 나오면 검사를 생략하지 않고
`BLOCKED.md`에 기록해 재제안한다. probe workflow는 최종 제품 변경에 남기지 않고
Linux 관통 테스트로 필요한 단정만 영구화한다.

### 2.4 자원 상한과 결정성

무제한 순회·hash·payload를 허용하지 않는다. production config는 다음 값을
명시하며 0/음수는 fail-closed다.

| 축 | 제안 기본값 | 초과 시 |
|---|---:|---|
| baseline+final scan node 수 | 100,000 | collection failure, process 미시작(baseline) 또는 upper 보존(final) |
| 변경 항목 수 | 10,000 | 부분 이벤트 없이 failure |
| 단일 regular file bytes | 256 MiB | 부분 hash 없이 failure |
| 전체 hash bytes | 2 GiB | failure |
| baseline manifest 메모리 | 64 MiB | failure |
| directory depth | 256 | failure |
| serialized fs payload | 4 MiB | writer 제출 전 failure |
| baseline/final scan 시간 | 각 60초 | bounded timeout, failure |

상한은 관측을 샘플링하거나 일부만 기록하기 위한 값이 아니다. 초과하면 전체 결과의
완전성을 증명할 수 없으므로 collector event와 ACK를 만들지 않고 upper를 보존한다.
파일 path, content 또는 symlink target을 오류에 그대로 출력하지 않고 상대 path만
진단한다. hash 결과와 `changes` 순서는 실행 순서와 map iteration에 무관하게
결정적이어야 한다.

### 2.5 upper가 보존하지 않는 일시 효과

upper 기반 fsdiff는 spawn 기준선과 종료 시점의 **순변화**를 관측한다. lower에
없던 파일을 run 중에 생성했다가 삭제하면 최종 upper에도 whiteout에도 흔적이
남지 않으므로 T11은 그 일시적인 생성·쓰기·삭제를 검출할 수 없다. 따라서
`collector/fs_changed`의 `changes:[]`는 “최종 순변화 없음”이지 “run 중 파일시스템
행위 없음”을 뜻하지 않으며, T12 감사도 이를 행위 부재의 증거로 사용하면 안 된다.
이 공백은 upper scanner를 추측으로 확장해 메우지 않는다. 파일 관련 syscall/exec
시계열을 관측하는 FR-COL-04(eBPF 기반 exec 감사, v2)가 보완 대상이다.

---

## 3. Q3 — agent 협조 없이, 비정상 종료에도 수집 (FR-COL-01/05)

collector는 adapter 이벤트를 수집의 trigger나 사실 근거로 사용하지 않는다.

1. `Prepare` 뒤 container 시작 전에 lower baseline을 만든다.
2. `Activate` 직후, host adapter를 시작하기 **전에** egress pump를 시작한다.
3. egress pump는 `Lease.Effects()`의 opaque delivery를 계속 소비하고 각 attempt를
   `collector/egress` 후보로 변환한다. surface가 writer ACK를 받아 delivery를
   resolve하기 전까지 proxy의 audit 제출 ACK를 보류한다. 따라서 writer
   backpressure가 collector→T10 broker→proxy→agent 요청까지 전파된다.
4. normal done, done 없는 adapter EOF, non-zero exit, stop, parent cancel, budget
   timeout 어디서든 같은 host-side finalizer를 `defer`로 실행한다.
5. finalizer는 caller 취소와 분리된 bounded context로 lease process를 멈추고
   effect stream EOF까지 drain한다. 그 뒤 lower 불변 재검사와 upper scan을 한다.
6. `collector/fs_changed`를 항상 한 건 기록한다. 변경이 없어도 `changes:[]`를
   기록해 “성공적으로 수집했음”과 “수집이 실행되지 않음”을 구분한다.

`subagent/done`은 정상 수집의 선결 조건이 아니다. agent/container가 먼저 죽어도
upper는 T10 `Close`가 보존하고 audit broker는 accepted effect를 drain하므로 host
collector가 계속한다. agent 오류와 collection 오류는 `errors.Join`으로 둘 다
상위에 반환하며 하나가 다른 하나를 가리지 않는다.

writer가 terminal이 되거나 egress event schema 검증이 실패하면 해당 delivery를
negative resolve하고 world를 fail-closed 종료한다. 허용 요청을 계속 보내면서
collector 기록만 잃는 경로는 허용하지 않는다. 현재 T10 broker는 queue enqueue
시점에 proxy에 ACK하므로 이 성질을 아직 만족하지 않는다. T11에서 다음처럼
명시적으로 계약을 강화한다.

```text
ActiveLease.Effects() <-chan EffectDelivery
EffectDelivery.Attempt() EffectAttempt
world.CommitEffect(writer, delivery, collectorRecord) -> opaque EffectReceipt
ActiveLease.AcknowledgeEffect(EffectReceipt)           // proxy ACK
ActiveLease.RejectEffect(delivery, reason)             // proxy NACK + fatal
```

`EffectDelivery`와 receipt의 상관 token은 비공개이고 lease/span/attempt ID와 exact
collector payload hash에 결속한다. `CommitEffect`가 actor/kind/span/payload를
검증하고 writer durable seq를 받은 뒤에만 receipt를 발급한다. zero/cross/reused
receipt는 proxy ACK 전에 거부한다. `RejectEffect`는 writer ACK를 가장하는 우회가
아니라 pending proxy 요청을 실패시키고 broker terminal 원인을 보존하는 전용
negative 경로다.

writer gate를 닫았을 때 **첫 요청부터** 완료되지 않아야 한다. bounded queue가
차서 나중 요청만 막히는 것은 충분하지 않다. gate 해제 뒤 event commit과 receipt
소비 후에만 proxy가 allow dial 또는 deny 응답을 진행한다. 현재
`EffectAttempt.ID`는 host-side delivery 상관에만 쓰고 agent payload로 노출하지
않는다.

agent process 사망과 HX host process crash는 구분한다.

- **agent/container 사망:** host finalizer가 살아 있으므로 scan→durable ACK→cleanup을
  수행한다. upper가 영구 누수되지 않는다.
- **HX host process crash:** 자동 삭제하지 않는다. upper는 수집되지 않은 증거일 수
  있으므로 보존이 fail-safe다. FR-COL-01의 agent 비정상 종료 범위 밖이며, v0.1에
  근거 없는 TTL GC를 넣지 않는다. 재기동 시 현재 session log와 trace/span이
  결속된 복구 경로가 없는 상태에서 삭제하면 증거 유실이므로, 이 상태는 명시적
  orphan 진단으로 남기고 별도 crash-recovery 계약 없이는 지우지 않는다.

마지막 항목은 “조용한 영구 누수”가 아니다. state root의 trace/span 디렉터리가
남고 cleanup은 실패로 보고된다. 다만 자동 재수집은 현행 CLI에 session resume
계약이 없어 T11 범위에서 지어내지 않는다.

---

## 4. Q4 — durable collection ACK와 upper 정리

### 4.1 egress delivery ACK와 fs collection ACK의 분리

두 ACK를 같은 타입이나 endpoint로 합치지 않는다.

- `EffectReceipt`: 한 proxy attempt의 durable `collector/egress` commit을 증명하고
  그 pending proxy request만 해제한다.
- `CollectionReceipt`: 종료 뒤 전체 fs scan의 durable `collector/fs_changed`
  commit을 증명하고 upper cleanup 권한을 연다.

egress receipt로 upper를 지우거나 collection receipt로 proxy 요청을 해제할 수
없다. 명목 타입과 비공개 token을 분리하고 cross-use 컴파일/런타임 회귀를 둔다.

### 4.2 lease-bound opaque collection receipt

T10 `SpawnReceipt`와 같은 패턴을 사용한다.

```text
collector fs scan
→ collector가 contracts/gen.EventRecord 후보 생성
→ surface가 world.CommitCollection(writer, lease subject, record) 호출
→ writer durable ACK(seq)
→ 비공개 필드의 CollectionReceipt 발급
→ 같은 ActiveLease.AcknowledgeCollection(receipt)
→ local backend가 package-private upper target 정리
```

`CollectionReceipt`는 lease identity, child span, fs payload hash와 durable seq를
비공개 필드로 결속한다. 외부 패키지는 zero receipt나 임의 필드 지정으로 위조할 수
없다. `CommitCollection`은 다음을 writer 제출 전에 검사한다.

- kind가 `collector/fs_changed`.
- actor가 정확히 `collector`.
- trace/span이 lease subject와 일치하고 parent span을 새 조인 경로로 쓰지 않음.
- raw가 합성 이벤트 규칙의 빈 bytes.
- payload가 collector가 산출한 canonical 순서이며 schema를 통과함.

zero/cross-lease/reused receipt, egress event receipt, writer ACK 없는 receipt는 upper
cleanup 전에 거부한다. cleanup 성공 뒤 같은 receipt 재호출은 idempotent success,
다른 receipt는 거부한다. cleanup이 실패하면 receipt를 소비 완료로 표시하지 않아
같은 receipt로 bounded retry할 수 있고 upper는 남는다.

### 4.3 cleanup capability

T10의 package-private `overlayCleanupTarget`에 승인 뒤 `upper` 한 값만 추가한다.
public API는 receipt만 받고 raw path를 받지 않는다. local lease가 이미 보유한
stateRoot/trace/span/layout으로 정확한 upper를 계산하고 T10과 같은 방어를 다시
적용한다.

- canonical containment와 exact expected path.
- stateRoot→upper ancestor 전부 `lstat`, symlink/non-directory 거부.
- `podman unshare rm -rf -- <exact upper>` argv.
- 사후 ENOENT 확인.
- state root 밖 sentinel 불변.

upper cleanup은 process stop, effect drain/ACK, container/network cleanup, work cleanup,
fs scan과 writer durable ACK 뒤의 마지막 단계다. 앞 단계 오류가 있거나 collector
receipt가 없으면 실행하지 않는다.

agent가 ACK 전에 죽어도 lease와 host collector는 죽지 않으므로 정상 finalizer가
receipt를 만든다. writer 실패·collector limit·path 검증 실패에서는 upper가 남는
것이 의도한 보존이며, 성공으로 가장하거나 빈 receipt로 지우지 않는다.

**회귀:** zero/cross/reuse receipt, writer 실패, scan 실패, symlink ancestor,
Podman 실패, 사후 잔존을 각각 구분한다. 모든 거부에서 upper와 state 밖 sentinel이
남고 cleanup runner 호출 횟수를 단정한다. 채널·cleanup 대기는 2초 단위 테스트
상한과 production bounded context를 가진다.

---

## 5. Q5 — collector/core 경계와 단일 writer (FR-COL-06)

두 요구는 surface 조립으로 동시에 만족한다.

```text
collector (stdlib + contracts)
    │  canonical gen.EventRecord 후보
    ▼
surfaces/hx collector coordinator
    │  world.CommitEffect / world.CommitCollection
    ▼
core/logd.Writer → append-only Store
```

- collector는 `core`, `seams`, `core/world`를 import하지 않는다.
- core와 seams도 collector를 import하지 않는다.
- collector는 DB 파일, SQLite seam, writer 내부 타입에 접근하지 않는다.
- surface만 world의 host-only source를 collector-owned 입력값으로 복사하고,
  collector의 contracts record를 기존 writer에 제출한다.
- 런타임 조인은 trace/span ID뿐이다. collector가 core의 replay state나 adapter
  intent를 조회하지 않는다.

즉 collector와 core가 같은 구현 경로를 공유하지 않지만, 신뢰된 composition root가
collector 출력의 **유일한 로그 진입점**으로 기존 writer를 사용한다. 별도 collector
DB, direct Store, 두 번째 writer는 만들지 않는다.

collector event envelope은 다음으로 고정한다.

```text
TraceID = session trace
SpanID  = 해당 subagent child span
Actor   = "collector"
Kind    = collector/fs_changed | collector/egress
Raw     = empty bytes (native NDJSON 원본이 없는 합성 관측)
Ts      = fs: finalize 시각, egress: EffectAttempt.AtUnixMs
```

boundarylint의 기존 양방향 금지(`collector→core/seams`, `core/seams→collector`)를
유지한다. 또한 collector가 승인되지 않은 외부 모듈을 직접 가져와도 현재의
`externalRestrictions` 목록에 없으면 통과하는 일반 구멍을 닫는다. `go list`
dependency metadata의 `Standard` 판정을 사용해 collector의 직접 import는
module 내부 `contracts|collector` 또는 표준 라이브러리만 허용한다. 경로에 점이
있는지를 표준 라이브러리 판정으로 추측하지 않는다.

다음 회귀를 추가한다.

1. `collector/fsdiff`가 `core/world`를 import하면 거부.
2. `seams/world/local`이 collector를 import하면 거부.
3. surface production package가 collector와 core/logd를 함께 import하는 조립은 허용.
4. collector package가 DB/store seam을 import하면 거부.
5. collector가 임의 외부 module을 import하면 목록 등록 여부와 무관하게 거부.
6. Linux-only와 test import에서도 같은 결과.

새 예외 규칙은 만들지 않는다. collector는 신규 외부 모듈 없이 stdlib만 사용한다.

**실패 모드:** collector가 writer를 우회해 DB를 열음, seams/world가 collector를
수평 호출, surface가 actor/span을 덮어씀, writer 실패 뒤 ACK·upper cleanup을
진행하는 경우다.

---

## 6. Q6 — 실제 Linux 통합 테스트

T10의 `make world-integration`/`ci-linux` gate를 확장한다. 별도 Fake 결과를
FR-COL 근거로 사용하지 않고 기존 FROM-scratch testagent와 rootless native
overlay/proxy를 그대로 쓴다. Linux/Podman/native-overlay 조건이 없으면 skip이
아니라 실패한다.

### 6.1 숨은 fs 변경

testagent는 native subagent message/tool event에 path를 보고하지 않은 채 다음을
실제로 수행한다.

- baseline regular file 수정.
- 새 regular file 추가.
- baseline file 삭제(whiteout).
- nested path에서 동일 세 동작.

surface finalizer 뒤 로그에서 정확히 한 `collector/fs_changed`를 찾고 다음을
단정한다.

- actor=`collector`, trace/child span 귀속.
- path 정렬, change_type과 SHA-256이 independently 계산한 기대값과 일치.
- deleted hash는 spawn baseline 값.
- lower hash 불변.
- whiteout은 owner를 보지 않고 `lstat(mode+rdev)`로 검출.
- 같은 path가 `subagent/tool_call`, `tool_result`, `message` payload에 없어서 실제
  “관측됐으나 보고 없음” 입력임.
- fs event durable ACK 전 upper 존재, ACK 뒤 upper ENOENT.

abnormal-exit subtest도 done 없이 숨은 파일을 만든 뒤 종료하고 같은 collector
event가 남는지 검사한다. writer failure를 주입한 subtest는 fs event 0, receipt 0,
upper 보존, cleanup 호출 0을 함께 단정한다.

### 6.2 egress와 T10 양성 대조

기존 proxy 시도를 실제 `collector/egress` event로 바꿔 allow/deny decision, deny
reason, method/bytes/time, actor/span을 검사한다. body/header/credential과 secret이
payload/raw 어디에도 없음을 전체 로그에서 재검사한다.

T10 잔여 지적도 같은 커밋에서 닫는다. IP literal 부재만 보지 않고 testagent의
실제 `direct-ip-denied` message를 로그에서 먼저 관측한 뒤 direct dial 실패를
단정한다. `direct_address` 누락·파싱 실패로 아무것도 시험하지 않고 green이 되는
경로를 회귀로 만든다.

### 6.3 backpressure와 자원 고갈

- writer gate를 닫으면 collector egress 제출이 block되고 T10 bounded stream을
  거쳐 proxy 요청도 완료되지 않는다. gate 해제 뒤 commit되고 유실 0.
- scan node/hash/payload 상한 초과는 부분 fs event 없이 실패하고 upper를 보존.
- collector output channel, effect drain, receipt, cleanup 대기에 상한.
- deny·writer failure에서 실제 external dial 0.

최종 CI 근거는 `make ci-linux`의 원격 run ID로 남긴다. macOS에서
`make world-integration`은 “테스트 없음” green이 아니라 기존의 명시 실패를
유지한다.

---

## 7. 구현 순서

1. SCP-T11-001 [H] 승인.
2. opaque/rename/symlink/hardlink 일회성 Linux probe; 결과와 run ID 리뷰.
3. 명세·schema·codegen·기존 sample을 원자적으로 변경하고 drift gate green.
4. `collector/fsdiff` baseline/final scanner와 한도·결정성 단위 테스트.
5. `collector/egress` mapper와 collector record envelope 테스트.
6. surface coordinator: pre-Activate baseline, pre-adapter egress pump, 모든 종료 경로
   finalizer, 단일 writer 제출.
7. lease-bound `EffectReceipt`/`CollectionReceipt`와 local upper cleanup capability,
   오류·위조 회귀.
8. boundarylint 회귀와 traceability 갱신.
9. T10 Linux 관통 gate 확장, IP literal 양성 대조, 원격 `make ci-linux` green 확인.

각 단계에서 `make ci`를 먼저 통과시키고 커밋을 분리한다. contracts 변경은 3단계
한 원자적 커밋 외에 넓히지 않는다.

---

## 8. 승인 요청

| 번호 | 승인 요청 | 변경 성격 |
|---:|---|---|
| 1 | SCP-T11-001: FR-COL-03을 proxy가 관측한 allow/deny 시도로 명확화하고 egress payload에 required `decision`, deny-only required `reason` 폐쇄 분기 추가 | 명세·contracts 비호환 강화 [H] |
| 2 | process 시작 전 lower 전체 baseline + 종료 뒤 전수 불변 재검사; 외부 변경은 정상 diff가 아니라 failure | fsdiff 의미론 |
| 3 | rename=delete+add, hardlink path별 기록, symlink domain-separated hash, opaque subtree 삭제 전개, 기타 special node fail-closed | 현행 3종 payload 안의 표현 규칙 |
| 4 | 표의 node/hash/change/payload/depth/time 상한과 초과 시 부분 기록 없는 failure | 자원 고갈 정책 |
| 5 | agent 종료와 무관한 host finalizer; egress pump는 adapter 전 시작, fs scan은 runtime quiesce 뒤 실행 | FR-COL-01 lifecycle |
| 6 | 한 attempt의 writer ACK 전 proxy ACK를 보류하는 opaque `EffectDelivery/EffectReceipt`; negative resolve는 broker fatal | audit-before-durable-write 계약 강화 |
| 7 | writer ACK로만 발급되는 lease-bound `CollectionReceipt` 뒤 upper 정리; raw-path cleanup API 금지 | T10→T11 cleanup 계약 |
| 8 | collector는 stdlib+contracts만, surface가 collector record와 core writer를 조립; boundarylint가 collector의 모든 non-stdlib 외부 import를 거부 | collector/core 경계 해석 |
| 9 | 실제 opaque 계열 표현은 구현 전 일회성 Linux probe가 표와 일치해야 하며 불일치 시 정지·재제안 | 실물 검증 gate |

신규 외부 의존성 승인은 요청하지 않는다.

## 9. 명시적 비범위

- T12의 의도/효과 대조 리포트와 불일치 분류.
- T13 확장 프로비저닝.
- T15 Claude Code 컨테이너 경로와 자격증명 주입.
- exec/eBPF 감사(FR-COL-04, v2).
- HX host process crash 뒤 session resume/자동 orphan 재수집. 증거 upper는 지우지
  않으며 별도 recovery 계약 없이 TTL GC를 만들지 않는다.
