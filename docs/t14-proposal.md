# T14 제안서 — OTel export, Codex 어댑터, `dump-config`

상태: 구현 전 제안. 이 문서는 contracts·go.mod·fixtures를 변경하지 않는다.

## 범위와 불변식

T14는 로그를 진실 원천으로 유지하면서 OTel을 파생 export로 제공하고, T8에서
녹화된 Codex NDJSON만 정규화하며, 병합된 프로파일을 안전하게 출력한다. export
실패는 세션·writer·정책 판정을 막지 않는다. 모든 출력과 export는 동일한 입력
로그에서 결정적으로 재계산된다.

## Q1. OTel 의존성과 ID 호환성

### 현재 ID 확인

`core/logd/ids.go`의 `NewTraceID`는 16바이트를 소문자 hex로 출력하고
`NewSpanID`는 8바이트를 소문자 hex로 출력한다. contracts도 trace_id 32자리,
span_id 16자리 소문자 hex와 all-zero 거부를 강제한다. 이는 OTel
`trace_id`(128-bit)·`span_id`(64-bit)의 텍스트 표현과 그대로 호환된다. 변환,
패딩, 재발급을 하지 않고 로그 값을 SpanContext에 넣는다. all-zero 방어와
소문자 검증은 이미 FR-OBS-01의 계약 테스트가 보장한다.

### 후보 비교

| 후보 | 실측/실패 모드 | 판단 |
|---|---|---|
| 공식 OTel SDK + OTLP/HTTP exporter | scratch 모듈에서 `go get go.opentelemetry.io/otel/sdk go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` 후 `go mod graph`가 239개 edge를 출력했다. SDK·exporter·protobuf·grpc 및 x/*의 버전·보안 갱신을 지속해야 한다. | 규격·Jaeger 호환성과 SpanData/BatchSpanProcessor를 얻지만, 신규 의존성 승인 없이는 사용하지 않는다. |
| OTLP/HTTP JSON 직접 구현 | stdlib의 `net/http`·`encoding/json`만으로 POST는 가능하다. 그러나 OTLP JSON의 AnyValue/Status/Resource/배치·재시도·압축·규격 변경을 직접 유지해야 하며 잘못된 필드가 수신기에서 조용히 버려질 수 있다. | 의존성은 0이지만 유지 부담과 호환성 위험이 커서 기본안으로 채택하지 않는다. |

**선택안:** [H] 승인을 전제로 공식 OTel SDK의 OTLP/HTTP exporter를 사용한다.
SDK는 `trace.NewNoopTracerProvider`에서 시작해 export가 구성된 경우에만
`BatchSpanProcessor`를 붙인다. 승인되지 않으면 go.mod를 건드리지 않고
BLOCKED.md에 남긴다. exporter endpoint·timeout·queue 크기는 설정으로 받고,
credential/header/body는 span attribute에 넣지 않는다. OTLP/gRPC는 선택하지
않고 HTTP 단일 경로로 의존성·방화벽 표면을 줄인다.

## Q2. Export는 파생 뷰

exporter는 `Reader`가 읽은 검증 완료 snapshot을 순회해 SpanData를 만들며
writer·SQLite·upper를 쓰지 않는다. 각 로그 이벤트의 envelope(seq, ts,
trace_id, span_id, parent_span_id)를 그대로 span context로 사용한다. 부모가
없는 session/start가 root span이고, parent_span_id가 있으면 child span이다.

export 실패는 stderr 진단과 내부 counter로만 남기고 세션 성공, append, replay를
실패시키지 않는다. 동일 snapshot을 다시 export하면 동일한 trace/span 관계와
속성이 생성된다(수신 시각·export 시도 ID 같은 비결정 값은 span attribute로
넣지 않는다). 리포트와 마찬가지로 로그를 변경하거나 보정 이벤트를 만들지
않는다.

## Q3. Span 속성의 출처

필수 속성은 로그에 이미 존재하는 값만 사용한다.

| 속성 | 출처 | 부재 처리 |
|---|---|---|
| adapter 종류 | `subagent/ready` actor 또는 spawn payload의 adapter | 해당 이벤트가 없으면 빈 값이 아니라 export 오류로 보고하고 해당 span만 생략한다. |
| adapter 버전 | Codex는 T8 meta의 `codex-cli 0.147.0`; Claude는 T8 골든 meta의 버전 | 로그 이벤트에 버전이 없는 임의 실행은 버전을 지어내지 않고 `unknown`으로 채우지 않으며 export 진단에 기록한다. |
| profile ID | `subagent/spawn` payload의 `profile_id` | 계약상 필수인 sandbox spawn에서 누락이면 span 생성 실패로 처리한다. |
| usage | `subagent/usage` 및 `logd.Replay`의 checked usage projection | projection 오류·overflow는 export 실패이며 0으로 대체하지 않는다. |
| 종료 상태 | `subagent/done` payload의 status | done이 없으면 아직 종료되지 않은 span으로 남기거나, 세션 종료 snapshot에서는 incomplete 오류로 보고한다. |

`raw`, instruction, prompt, credential, request body/header, 파일 내용은 속성에
넣지 않는다. span name과 status도 고정된 제한 집합에서만 선택한다.

## Q4. §8-7 Jaeger 검증

외부 인터넷이나 SaaS를 사용하지 않고 CI job에서 Jaeger all-in-one 컨테이너를
띄운다. Jaeger의 OTLP/HTTP 수신 포트를 job-local 네트워크에 노출하고, 고정된
로그 snapshot을 exporter로 전송한 뒤 Jaeger query API에서 trace를 조회한다.

자동 게이트는 다음을 단정한다.

1. trace ID가 로그의 32hex와 동일하고 all-zero가 아니다.
2. session/root span 아래 spawn child span이 존재한다.
3. child의 parent_span_id가 정확히 부모 span이고, sibling·child 이벤트가
   부모 span으로 합쳐지지 않는다.
4. profile_id, adapter 종류·버전, usage, 종료 상태가 속성에 존재한다.
5. 동일 snapshot을 두 번 export한 결과의 span tree와 속성이 byte-identical
   정규화 비교를 통과한다.

Jaeger UI의 색상·레이아웃을 사람이 눈으로 확인하는 절차는 자동 게이트의
대체물이 아니며 [H] 선택 검수로만 남긴다. CI는 Linux에서 Jaeger image
pull/build 실패를 skip하지 않고 실패시킨다. 이미지 digest와 job/run ID를
PR에 기록한다.

## Q5. Codex 어댑터 (FR-ADP-09)

T8의 유효 Codex 녹화는 7건이다(01, 02, 03, 04, 06, 07, 08; codex-cli
0.147.0). 05는 NDJSON 녹화가 없고 `codex exec --json`에서 승인 UI가
표면화되지 않았다는 meta-only 기록이다. 따라서 T14는 픽스처에 나타난
`command_execution`, `file_change`, 완료/오류/중단 상태만 정규화한다.

구조는 Claude 어댑터와 동일하게 순수 parser → 정규화 이벤트 → golden 및
fingerprint 대조로 나눈다. Codex의 비대화형 출력에 승인 request가 없다는
사실을 승인 handshake로 일반화하지 않는다. `manual` 정책에서 실행 이벤트가
나오지만 승인 경로가 관측되지 않으면 허용을 추측하지 않고 fail-closed
(명시적 adapter 오류와 done/error)로 처리한다. `auto`도 프로파일이 명시한
경우에만 가능하며, Codex가 승인을 제공한다고 가장하지 않는다.

관측 등급은 픽스처가 보여주는 구조적 이벤트가 있는 시나리오에서
`observable`로 선언한다. 승인 이벤트·실시간 상호작용·픽스처에 없는 tool
형식은 지원한다고 선언하지 않으며, 해당 시나리오는 meta-only 사유로 남긴다.
픽스처 원본과 fingerprint는 수정하지 않고, 7건 모두 parser golden과 미매핑
라인 사유를 요구한다.

## Q6. `hx dump-config` (FR-CLI-05)

`hx dump-config`는 프로파일을 기존 parser와 `policy.Merge`로 병합한 뒤,
실제로 Backend/adapter에 전달될 최종 트리를 stdout에 출력한다. 새로운 병합
경로를 만들지 않고 `policy.Evaluate` 결과를 사용한다.

출력 트리는 다음 축을 포함한다: profile 계보/ID, fs scope의 정규화 경로,
egress(교집합), allowed extensions/registries(교집합), resolved extension
version·integrity·source·artifact digest, budget, approval mode, world backend,
image digest, mount target/mode. host-only `UpperDir`, cache root, socket,
Podman endpoint는 출력하지 않는다.

credential handle, bearer token, authorization/header/body, 환경변수의 비밀 값은
항상 `<redacted>` 또는 구조적 부재로 표시한다. map key와 list는 canonical
정렬하고 JSON을 고정 indent/newline으로 직렬화한다. 같은 profile 입력은
실행 시각·map 순회와 무관하게 byte-identical이다. 잘못된 profile, 중복 키,
병합 거부는 stdout 0바이트·non-zero exit, 진단은 stderr로 보낸다.

## 구현·검증 순서

1. [H] OTel 의존성 승인 및 scratch의 239-edge 측정 기록 확인
2. contracts 입력 decoder와 로그 snapshot → SpanData 변환, ID/parent/속성
   단위 테스트
3. Codex 7개 golden/fingerprint parser 및 미매핑·승인 부재 회귀
4. `dump-config` surface 연결, redaction·결정성·오류 stdout 0바이트 회귀
5. Linux CI에서 Jaeger container/API 관통 테스트. exporter 실패·Jaeger
   부재는 세션을 막지 않는다는 단위 테스트와 별도로, 통합 게이트에서는
   skip 없이 실패.

## 승인 요청

| 항목 | 승인 필요 이유 | 범위 |
|---|---|---|
| 공식 OTel SDK + OTLP/HTTP exporter | 신규 Go 모듈과 239-edge 의존성 그래프가 발생하며 CLAUDE.md상 [H] 승인 필요 | 고정 버전 SDK/HTTP exporter만, gRPC·자동 계측·UI 제외 |
| FR-OBS-01/02 export 계약 | 로그 파생 SpanData, ID/parent/속성 매핑 확정 | export 실패가 세션을 막지 않는 비동기 read-only 경로 |
| FR-ADP-09 Codex parser | T8 7개 픽스처 범위와 approval 미지원 fail-closed | 픽스처 밖 동작·실 인증·승인 UI 구현 제외 |
| FR-CLI-05 dump-config | 최종 병합 트리·redaction·결정적 직렬화 표면 | contracts 변경·host secret/경로 출력 제외 |
| §8-7 Jaeger Linux gate | 로컬 컨테이너 및 query API 기반 자동 검증 | 외부 레지스트리·사람 UI 검수는 사용하지 않음 |

