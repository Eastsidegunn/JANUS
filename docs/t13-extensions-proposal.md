# T13 확장 패스스루 제안 — 프로비저닝과 실행 프로파일 분리

상태: **제안서 / 구현 전** (2026-08-31)

이 문서는 FR-EXT-01~08과 수용 기준 §8-8을 구현하기 전에 계약과 실패
의미론을 확정한다. 이번 변경에는 Go 코드, `contracts/`, 픽스처, 워크플로
변경이 없다. 현재 task 와이어에는 확장 선언이 이미 있지만, T10에서 폐쇄한
`subagent/spawn` metadata에는 설치 결과를 표현할 필드가 없으므로
SCP-T13-001 승인을 먼저 요청한다.

## 0. 결론

| 질문 | 선택한 안 |
|---|---|
| Q1 선언 | 요청의 구체적인 확장은 `SpawnSpec`/task에, 허용 이름·레지스트리 정책은 `Profile`에 둔다. 버전과 `sha256:` 무결성은 항상 고정한다. |
| Q2 단계 분리 | 프로비저닝 전용 일회성 컨테이너와 registry-only egress를 사용하고, 종료·검증한 뒤 별도의 실행 컨테이너를 만든다. registry allowlist는 자동 승계하지 않는다. |
| Q3 기록 | 프로비저닝 성공으로 확정한 설치 세트를 `CommitSpawn` 직전 spawn metadata에 넣는다. 실패한 시도는 spawn을 만들지 않고 기존 policy/decision 진단으로 남긴다. |
| Q4 병합 | 허용 확장 이름과 registry allowlist는 정책 프로파일 간 교집합이다. 요청된 항목이 교집합 밖이면 조용히 제거하지 않고 spawn을 거부한다. |
| Q5 실행 egress | 선언의 `egress` 중 병합 정책을 통과한 도메인만 실행 프로파일에 넣는다. source registry는 별도 집합으로 유지한다. |
| Q6 캐시 | 콘텐츠 주소 캐시를 사용한다. 해시 검증·원자적 저장·읽기 전용 제공을 지키고 불일치는 fail-closed 한다. |
| Q7 검증 | 외부 레지스트리에 의존하지 않는 CI 전용 로컬 registry/artifact service를 실제 컨테이너로 띄워 §8-8을 Linux에서 반복 검증한다. |

### 공통 보안 경계

확장 코드는 비신뢰 코드다(FR-EXT-07, NFR-06). 프로비저너와 실행 에이전트에는
host adapter binary, approval socket, Podman socket, host 자격증명을 mount하지
않는다. 프로비저너가 쓰는 디렉터리는 host state root 아래의 확장 bundle뿐이고
워크스페이스 lower/upper는 쓰기 가능하게 주지 않는다. 따라서 설치 결과가
워크스페이스 변경으로 위장되지 않으며, T11 fsdiff가 보지 못하는 설치 파일을
보고했다고 가장하지 않는다. 설치 세트와 결과물 digest는 spawn metadata가
재현 근거가 되고, 실행 중 워크스페이스 변경과 egress 시도는 기존 T11 collector
관측 대상이다.

---

## 1. Q1 — 확장 선언 형식 (FR-EXT-01/02)

### 현재 입력과 역할 분리

현재 `contracts/wire.schema.json`의 task `extensions` 항목은 다음 선언을
받는다.

```text
name, version, integrity, source, egress[]
```

이 선언은 task decoder에서 검증한 뒤 (현재 core 타입에는 아직 필드가 없으므로
구현 단계에서 추가할) 불변 `SpawnSpec.Extensions`에 보존되어 어댑터가 대상
에이전트에 전달할 요청이다. 제안서 단계에서는 이 타입 변경을 하지 않는다.
정책 프로파일에는 (구현 단계에서 추가할) 다음 두 제한을 별도로 둔다.

```text
allowed_extensions[]   # 허용된 확장 이름 또는 이름+출처 식별자
allowed_registries[]   # 프로비저닝 중 허용할 registry host
```

둘을 한 필드에 섞지 않는 이유는 요청은 구체적인 설치 의도이고 프로파일은
그 의도를 좁히는 상한이기 때문이다. `SpawnSpec`만 검사하면 호출자가 정책을
우회하고, Profile만 검사하면 실제로 무엇을 설치했는지 재현할 수 없다.

### 고정 규칙

- `version`은 정확한 버전 문자열만 허용한다. `latest`, 범위(`^1`, `>=1`),
  빈 값은 기본 거부한다. 정책이 비어 있거나 알 수 없는 경우도 거부한다.
- `integrity`는 `sha256:` 뒤 소문자 hexadecimal 64자리인 콘텐츠 digest다.
  tag나 알고리즘 생략은 허용하지 않는다.
- `source`는 선언에 적힌 registry 식별자이며, 프로비저닝 allowlist와
  canonical host 비교를 거친다. 선언의 URL 문자열을 그대로 shell 인자나
  proxy 우회 경로로 사용하지 않는다.
- 같은 `(name, version, integrity, source)` 선언이 한 spawn에 중복되면
  모호한 설치 세트를 만들지 않고 거부한다. `egress`도 canonicalize 후
  정렬·중복 제거하고, 잘못된 항목이 있으면 전체 선언을 거부한다.

선택지는 (a) task만, (b) Profile만, (c) 둘 다였다. (c)를 선택한다. 정책
교집합과 concrete declaration 양쪽을 검사해야 “허용된 이름이지만 다른 해시를
설치”하는 혼동과 “요청은 없지만 프로파일에 있으니 설치”하는 과잉 권한을
동시에 막을 수 있다.

실패 시점은 프로비저너 실행 전이다. 버전 미고정·해시 형식 오류·허용되지 않은
이름/registry·중복은 registry 접속, 파일 생성, container start가 모두 0이고
기존 spawn durable event도 만들지 않는다.

---

## 2. Q2 — 프로비저닝/실행 프로파일과 단계 경계 (FR-EXT-03)

### 선택지

| 안 | 판단 | 실패 모드 |
|---|---|---|
| 같은 컨테이너에서 proxy allowlist만 교체 | 탈락 | allowlist 교체와 프로세스 실행 사이 경합에서 registry egress가 실행 단계로 새거나, agent가 전환 시점의 권한을 잡는다. |
| 같은 컨테이너를 stop/restart | 탈락 | 이전 namespace·파일·프로세스가 남는 실패 경로가 생기고, “프로파일 교체 완료”를 독립적으로 증명하기 어렵다. |
| **일회성 provisioning container + 별도 execution container** | **선택** | 단계가 컨테이너·network namespace·프로세스 수명으로 분리된다. 프로비저너가 끝나고 검증된 뒤에만 실행 world가 생성된다. |

선택한 순서는 다음과 같다.

```text
policy merge/validate
  → provisioning container (registry-only internal bridge + proxy)
  → artifact hash 검증·bundle 봉인·provisioner 제거
  → execution profile 계산
  → execution container (T10 internal bridge + 실행 egress만)
```

프로비저너는 registry host만 허용하는 proxy를 사용한다. 실행 컨테이너는
프로비저너의 network namespace나 proxy endpoint를 공유하지 않는다. 설치
bundle은 host state root의 lease-bound 경로에 만들고 실행 컨테이너에는 필요한
bundle만 read-only로 전달한다. 프로비저너가 workspace를 변경하려고 하면
workspace mount 자체가 없거나 read-only이므로 성공할 수 없고, 그 사실을
정상 설치로 처리하지 않는다.

프로비저닝이 성공해도 registry host는 실행 allowlist에 자동 추가되지 않는다.
실행 단계는 새 proxy allowlist로 시작하며, 프로비저너의 모든 프로세스·소켓·
network가 제거된 ACK가 전환의 전제다. 두 단계가 동시에 살아 있는 시간은
없다. 따라서 allowlist를 바꾸는 순간의 경합으로 registry 권한이 실행 단계에
남는 경로가 없다.

T10의 `Prepare → CommitSpawn → Activate`와 결합할 때는 다음 순서를 사용한다.

```text
policy 평가·선언 검증
→ provisioner 성공 및 결과 digest 확정
→ world.Prepare (실행 overlay/network 준비, 아직 start 없음)
→ lower baseline
→ resolved extension metadata를 포함한 spawn durable CommitSpawn
→ world.Activate
→ execution container/adapter start
```

`CommitSpawn`이 실패하면 Activate와 두 번째 container start가 모두 0이다.
프로비저너 실패는 bundle을 폐기하고 spawn을 만들지 않는다. 프로비저너가
비정상 종료·timeout·registry deny를 냈는데도 partial bundle을 실행에 넘기지
않는다.

---

## 3. Q3 — 설치 세트와 spawn metadata (FR-EXT-04, SCP-T13-001)

### 기록 시점

T10은 spawn event를 durable ACK한 뒤에만 Activate한다. 따라서 “요청 선언”과
“실제 설치 결과”를 같은 시점에 추측해서 쓰지 않는다.

1. 요청 선언은 task/`SpawnSpec.extensions`에 들어온다. 이 값은 의도이며 설치
   성공의 증거가 아니다.
2. 프로비저너가 registry에서 받은 bytes를 digest로 검증하고 bundle을 봉인한
   뒤, 실제 설치 세트 `(name, version, integrity, source)`와 각 결과물
   `artifact_digest`를 확정한다.
3. 확정된 결과를 담은 `subagent/spawn` metadata를 만든 뒤 `CommitSpawn`의
   writer ACK를 받는다. 그 다음에만 Activate한다.

따라서 성공한 extension spawn의 spawn event 자체가 설치 세트와 결과물 hash를
가지며, replay/fork는 그 event를 읽어 같은 bundle digest를 요구할 수 있다.
프로비저닝 실패에는 성공한 spawn event를 만들지 않는다. 실패 원인은 기존
`policy/decision`/adapter error 경로의 durable record로 남기되, 실패한 bundle을
“설치됨”으로 기록하지 않는다.

### SCP-T13-001 제안

현재 `subagentSpawnPayload`가 T10에서 `additionalProperties:false`로 폐쇄되어
있으므로 다음을 한 원자적 contracts·codegen·샘플 갱신으로 제안한다.

- `world_backend:"none"` 분기는 확장 metadata를 요구하지도 허용하지도 않는다.
  null/test spawn의 기존 의미를 보존한다.
- `world_backend:"local-podman"`은 두 형태로 나눈다.
  - extension 없음: 기존 필드만 허용한다.
  - extension 있음: `extensions` 배열을 required로 하고, 각 항목에
    `name`, 정확한 `version`, `integrity`(`sha256:` 64 hex), `source`,
    `artifact_digest`를 required로 한다.
- 두 local 분기 모두 전체 허용 필드를 반복하고 `additionalProperties:false`를
  유지한다. 따라서 비샌드박스 분기에 extension 필드를 강제하지 않으면서,
  extension-bearing spawn은 결과 hash 없이 저장될 수 없다.
- `extensions` 배열은 deterministic name/version/source 순서와 중복 금지를
  validator에서 확인한다. 민감한 credential, bearer token, 원본 header/body,
  host absolute path는 metadata에 넣지 않는다. bundle은 digest와 lease-bound
  `upper_ref`/extension ref로만 재현한다.

이 SCP는 기존 T10/T11 폐쇄와 충돌하는 비호환 schema 강화다. 승인 전에는
`contracts/`를 수정하거나 생성 타입을 만들지 않는다. 승인 후에도 schema,
emitter, codegen, validate/replay 샘플을 한 커밋으로 갱신한다.

### 설치 결과와 T11 collector

프로비저너의 bundle은 workspace upper가 아니라 lease state root의 전용
extension 경로에 생긴다. 따라서 T11 fsdiff의 `changes:[]`를 “설치 행위가
없었다”는 증거로 사용하지 않는다. 설치 세트와 결과물 digest는 spawn metadata가
담당하고, 실행 중 workspace 파일 효과는 T11 fsdiff, 네트워크 시도는
`collector/egress`가 담당한다. 확장 bundle은 실행 중 read-only라 agent가
그 metadata와 실제 bytes를 바꿀 수 없다. bundle을 지우거나 hash를 바꾸는
시도는 실행 시작 전 검증 실패 또는 별도 효과 오류가 된다.

---

## 4. Q4 — 정책 병합 (FR-EXT-05, FR-POL-03)

각 Profile은 `allowed_extensions`와 `allowed_registries`를 선택적으로 가질 수
있다. 병합 결과는 다음과 같다.

```text
effective_extensions = ⋂ profile.allowed_extensions
effective_registries = ⋂ profile.allowed_registries
```

어떤 profile이 목록을 생략한 경우를 “전부 허용”으로 해석하지 않는다. 정책
작성자가 명시하지 않은 capability는 빈 집합(deny)로 시작하고, 상위 계약이
별도로 “무제한”을 정의할 때만 그 의미를 사용한다. auto approval, 예산,
workspace scope와 같은 T6 규칙도 그대로 좁아지기만 한다.

요청 extension이 `effective_extensions` 밖이거나 source가
`effective_registries` 밖이면 프로비저너를 시작하지 않고 명시적 policy deny를
기록한다. 일부 항목만 조용히 떼고 나머지를 실행하는 방식은 요청자가 어떤
환경에서 실행되는지 숨기므로 사용하지 않는다.

속성 테스트 계획:

- 두 profile을 합친 결과가 어느 입력보다 확장·registry 집합을 넓히지 않는다.
- profile 순서를 바꿔도 결과가 같고, 세 profile 체인에서도 교집합이 유지된다.
- 병합 후 허용되지 않은 요청은 registry 접속·파일 생성·container start가
  0이다.
- 명시적 `egress`도 정책 교집합 밖이면 실행 profile에 들어가지 않는다.

---

## 5. Q5 — 실행 중 네트워크 확장 (FR-EXT-06)

확장 선언의 `source`와 `egress`는 서로 다른 의미다.

- `source`: 설치 bytes를 받는 프로비저닝 registry. provisioning-only 집합이다.
- `egress`: 설치된 확장이 실행 중 필요로 하는 host/domain. 실행 profile에
  넣을 후보 집합이다.

실행 profile에는 `egress ∩ effective_policy.egress`만 넣는다. 선언된
`egress`가 정책 밖이면 해당 확장을 실행하지 않고 spawn을 거부하거나(정책이
확장 전체를 필수로 표시한 경우), 명시적 deny 결과로 끝낸다. “요청을 받았지만
실행 allowlist에서 빠졌다”를 성공 설치로 위장하지 않는다.

source registry가 egress에도 적혀 있다는 이유만으로 자동 편입하지 않는다.
두 목록에 같은 host가 모두 있고 정책도 허용한 경우에만 실행 단계에서
허용된다. 그렇지 않으면 실행 proxy는 registry 접근을 차단하고 deny attempt를
기록한다. 프로비저닝 proxy의 allowlist를 복사하는 코드는 만들지 않는다.

실행 단계에서는 T10의 route 부재와 audit-before-dial을 그대로 사용한다.
확장이 `HTTP_PROXY`를 지워도 직접 외부 route가 없으므로 우회가 아니라 실패가
된다. allowlist 전환은 컨테이너 교체로만 일어나며, 이전 proxy가 살아 있는
동안 새 agent를 시작하지 않는다.

---

## 6. Q6 — 콘텐츠 주소 캐시 (FR-EXT-08)

반복 spawn 비용을 줄이기 위해 캐시를 **도입한다**. 캐시는 host-only state
root 아래에 두며 key는 `sha256:<64 hex>`다. source/name/version은 metadata와
검증 로그에 남기지만, 캐시 hit의 신뢰 근거는 bytes의 실제 SHA-256이다.

저장·조회 규칙:

1. 임시 파일에 받은 bytes를 쓰고 fsync한 뒤 digest를 계산한다.
2. 선언 digest와 다르면 임시 파일을 폐기하고 `cache_integrity_mismatch`로
   fail-closed 한다. 기존 캐시가 있어도 다른 bytes로 덮어쓰지 않는다.
3. 일치할 때만 원자적 rename으로 immutable entry를 만든다. 동시 writer는
   lock 또는 exclusive create로 한 entry를 소비하며, 검증되지 않은 partial
   file을 hit로 보지 않는다.
4. 실행 컨테이너에는 검증된 bundle을 read-only로만 제공한다. agent는 cache
   root, eviction metadata, registry credential에 접근하지 못한다.

캐시 포화·권한 오류·손상 index는 cache miss로 조용히 폴백하지 않는다. 해당
요청의 bytes를 다시 받아 검증할 수 있으면 miss로 재프로비저닝하고, 재검증도
실패하면 spawn을 만들지 않는다. eviction은 T13 범위의 성공 경로에서 하지
않으며, ACK 전에 설치 bundle을 삭제하지 않는다.

---

## 7. Q7 — §8-8 Linux 검증과 CI 배치

외부 npm/PyPI/인터넷 registry에 의존하면 DNS, rate limit, image 변경으로
게이트가 흔들리므로 사용하지 않는다. CI job 안에서 다음을 실제 rootless
Podman으로 구성한다.

1. 고정된 작은 extension artifact와 manifest를 job에서 생성한다. artifact의
   SHA-256과 이름/버전을 먼저 출력하고, registry 역할의 로컬 HTTP artifact
   service(또는 pinned local OCI registry container)를 외부 network 쪽에
   만든다. 이 service는 테스트 전용이며 production registry를 흉내 내는
   Fake가 아니라, proxy를 통한 실제 HTTP fetch의 대상이다.
2. provisioning container는 registry domain만 허용된 proxy를 통해 artifact를
   가져오고, 선언 digest 검증·bundle 설치·설치 결과 digest를 출력한다.
   허용 domain 접근 성공과 잘못된 digest 거부를 모두 단정한다.
3. provisioning container와 proxy를 정리한 뒤 execution profile로 새 agent를
   시작한다. execution 단계에서 같은 registry domain을 직접 IP와 hostname
   양쪽으로 시도하고, direct/registry 접근은 실패하며 deny audit가 남는지
   확인한다. `egress`에 별도로 선언·정책 허용된 domain은 proxy를 통해 성공한다.
4. spawn durable record에 설치 세트(이름·버전·hash·source)와 artifact digest가
   있고, replay/fork에서 같은 metadata가 재계산되는지 확인한다.

필수 관통 시나리오:

| 시나리오 | 기대 결과 |
|---|---|
| registry allow + 고정 digest | provisioning 성공, spawn metadata 기록, execution 시작 |
| registry 응답 bytes 변조 | digest mismatch, bundle·spawn·execution start 0 |
| 실행 중 동일 registry hostname | proxy deny + `collector/egress` deny record, dial 0 |
| 실행 중 registry IP literal | route/proxy 정책으로 실패, deny 또는 direct-attempt 관측, 성공 연결 0 |
| 명시적 실행 egress allow | proxy allow + allow record, credential/body/header 미기록 |
| 허용되지 않은 extension/registry | policy deny, provisioning side effect 0 |
| cache hit/miss | 실제 bytes 재검증 후에만 실행, 손상 cache는 fail-closed |

이 테스트는 `make extensions-integration`으로 명명하고 기존 `make smoke`의
CGO 없는 순수 Go 의미와 충돌시키지 않는다. `ci-linux`에 포함하며 Linux,
rootless, native overlay, Podman 전제 미충족은 skip이 아니라 실패다. 같은
commit SHA에서 최소 5회 연속 green을 요구하고 각 run ID를 PR에 기록한다.
아직 이 제안서에는 실행 결과나 run ID를 주장하지 않는다. 구현 후 실제 CI
프로브가 이 설계와 다르면 단정을 낮추거나 게이트를 빼지 않고 BLOCKED.md에
실패 사실을 기록한다.

### 측정·부작용 단정

각 거부 케이스는 오류뿐 아니라 registry dial, bundle 생성, container start가
0임을 함께 확인한다. 대기 채널에는 상한을 둔다. writer/audit queue 포화,
cache partial write, proxy ACK 실패에서도 조용한 성공이나 부분 metadata를
만들지 않는다.

---

## 승인 요청 — SCP-T13-001

| 항목 | 승인 필요 이유 | 범위 |
|---|---|---|
| spawn metadata extension 분기 | T10의 `additionalProperties:false` 폐쇄를 비호환 확장 | `world_backend:none`은 기존 폐쇄 유지; `local-podman`에 extension 없음/있음 두 분기 추가 |
| 설치 세트 필드 | FR-EXT-04의 이름·버전·hash·출처 및 provisioning 결과 hash를 spawn에 durable 기록 | `extensions[]`의 required 필드와 digest 검증 |
| 명세 문구 | FR-EXT-04가 설치 결과를 spawn metadata에 요구한다는 시점·순서 명확화 | §5.1 spawn payload와 §8-8 보강 |
| codegen·기존 샘플 | 원자적 schema 변경 없이는 writer 검증과 생성 타입이 어긋남 | 승인 후 한 커밋에서만 갱신 |

SCP 승인 전에는 contracts, fixtures, emitter, policy 구현을 변경하지 않는다.
승인 후에도 T13 구현 순서는 `선언·정책 → 프로비저너/프로파일 분리 →
metadata/codegen → 캐시 → Linux 관통 게이트`로 유지한다.

## 비범위와 정지 조건

- HX 자체 marketplace·curation, 확장 안전성 심사, T14/T15 구현은 비범위다.
- 실제 rootless network 또는 registry 동작은 문서 대조로 완료 처리하지 않고,
  Linux CI probe와 5회 반복 결과가 있을 때만 주장한다.
- extension bytes가 workspace/host socket/adapter를 넘는 경로, registry
  allowlist가 실행 단계로 승계되는 경로, 또는 spawn metadata에 설치 digest를
  넣을 수 없는 경우에는 우회하지 않고 BLOCKED.md에 기록한다.
