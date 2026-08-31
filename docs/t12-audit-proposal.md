# T12 `hx audit` 제안서 — 의도 평면과 효과 평면의 대조

상태: **승인 대기**. 대상: FR-AUD-01/02/03/04, FR-CLI-04.
이 PR은 제안서만 추가하며 Go 코드, `contracts/`, `fixtures/`, 워크플로를
변경하지 않는다. 리포트는 append-only 로그에서 계산하는 파생 상태이고 로그를
수정하거나 별도 진실 원장을 만들지 않는다.

## 0. Q1 선행 실측 — 두 평면은 결합된다

T11 Linux 관통 게이트 `33096358708` (commit
`26a2e500797c5792ece9bfb7100f9646e96d6ae5`, Ubuntu/rootless/native-overlay,
job `98616379426`)의 normal 시나리오를 근거로 삼는다. 로컬 macOS에는
Podman/native overlay가 없으므로 동일 실물 실행을 로컬에서 재현했다고 주장하지
않는다.

실측 시나리오는 한 실행에서 다음을 함께 수행했다.

* 의도 평면: `subagent/tool_call`의 `call_id=call-allow`가
  `{"path":"/workspace/approved-marker.txt"}`를 보고했다. 동일 child span으로
  approval request와 tool result가 기록된다.
* 효과 평면: allow 뒤 실제 `approved-marker.txt`가 생성되어
  `collector/fs_changed`의 `approved-marker.txt: added`가 되었고, 별도로
  보고되지 않은 `created.txt`, `modified.txt`, `deleted.txt`도 같은 이벤트에
  들어갔다. T11 gate는 이 집합과 child span 귀속, lower 불변, upper 정리를
  실제 Podman 실행에서 확인했다.

따라서 결합은 다음처럼 성립한다.

| 관측 | 결과 |
|---|---|
| 경로 | tool call의 `/workspace/approved-marker.txt`를 mount target prefix 제거 후 `approved-marker.txt`로 정규화하면 collector 상대 POSIX 경로와 일치한다. |
| span | tool call과 `collector/fs_changed` 모두 해당 subagent child span을 가진다. |
| 순서 | event `seq`로 tool call이 효과 이벤트보다 앞섬을 확인할 수 있다. timestamp는 동률·시계 보정 때문에 보조 정보로만 쓴다. |
| 유일성 | 경로와 span만으로는 같은 파일에 대한 반복 시도를 구분할 수 없으므로 `call_id`와 seq 순서를 포함한 one-to-one 매칭을 사용한다. |

이 실측은 “보고된 변경과 효과가 언제나 일치한다”는 뜻이 아니다. 오히려 같은
span의 숨은 파일 변경이 관측-무보고로 남는다는 점이 T12 대조의 입력을
제공한다.

## 1. Q1 — 결합 키와 정규화

### 1.1 의도 행위의 내부 표현

로그의 `subagent/tool_call`을 adapter별 decoder가 `IntentAction`으로 변환한다.
decoder는 tool name, `call_id`, child span, event seq, 원본 payload를 보존하고,
파일 효과를 주장할 수 있는 path 인자만 추출한다. 알 수 없는 tool이나 path 없는
tool은 파일 효과 의도로 만들지 않고 `non-filesystem intent`로 남긴다. audit은
어댑터가 보고한 문장을 임의로 파일 변경으로 추측하지 않는다.

### 1.2 canonical path

경로는 audit analyzer가 다음 순서로 canonicalize한다.

1. JSON 문자열을 UTF-8로 해석하고 NUL 및 빈 path를 거부한다.
2. POSIX 구분자로 바꾸고 `path.Clean`을 적용한다. `.`은 제거하지만 `..`가
   workspace root 밖으로 나가면 정규화 실패다.
3. `/workspace/` mount target의 정확한 경계 prefix를 제거해 상대 경로로 만든다.
   이미 상대 경로면 그대로 clean한다. `/workspace-evil/x` 같은 lexical prefix는
   허용하지 않는다.
4. 결과가 빈 문자열, 절대 경로, 또는 `.`이면 결합 불가로 표시한다.

Linux workspace는 대소문자를 구분하므로 case-fold하지 않는다. symlink target을
따라가거나 `EvalSymlinks`로 의도 문자열을 다른 파일로 바꾸지 않는다. symlink는
효과 평면에서 collector가 정의한 path 항목으로만 비교한다. 절대/상대 혼용,
`..`, 잘못된 mount prefix, NUL, 정규화 충돌은 일치로 처리하지 않고
`unmatchable_intent` 진단으로 남긴다.

### 1.3 매칭 알고리즘

효과 행은 `collector/fs_changed` payload의 change path와 event span/seq에서
읽는다. 기본 키는 `(child_span, canonical_relative_path)`이고, 같은 키가 여러
번 나오면 각 span 안의 seq 오름차순으로 intent와 effect를 one-to-one 소비한다.
가능한 경우 tool `call_id`와 change type(add/modify/delete)를 보조 제약으로
사용한다. 한 intent가 두 effect에 재사용되거나 한 effect가 두 intent에 소비되면
ambiguous로 실패하며 일치로 축약하지 않는다.

효과 event의 seq가 tool_call보다 앞서거나 span이 다르면 경로가 같아도 일치가
아니다. 시각은 설명용이며 결합 키가 아니다. child span이 없는 collector record,
또는 collector와 tool_call의 trace가 다르면 데이터 손상으로 리포트 생성에
실패한다.

## 2. Q2 — 세 분류의 정의

각 비교 가능한 파일 intent/effect 쌍은 다음 세 값 중 하나만 갖는다.

| 분류 | 판정 조건 | 의미 |
|---|---|---|
| `reported_unobserved` (보고-무관측) | canonical path·child span·one-to-one 조건을 만족하는 효과가 없고, 해당 intent가 정규화 가능함 | 환각 또는 거짓 보고 의심. 실제로 변경이 없었다고 단정하지 않는다. |
| `observed_unreported` (관측-무보고) | collector effect에 대응하는 정규화 가능한 tool intent가 없음 | 은폐·어댑터 누락 의심. T11의 hidden file 시나리오가 이를 만든다. |
| `matched` (일치) | 양쪽 행이 같은 child span과 canonical path로 한 번씩 소비되고, 허용된 change type 관계도 만족함 | 보고된 파일 행위와 관측된 순변화가 대응함. |

정규화 실패, duplicate/ambiguous 매칭, lower 재검사 실패, scan 상한 초과,
`changes:[]`만 있고 관측 완전성이 확인되지 않은 경우는 세 분류 중 하나로
강제하지 않는다. 리포트는 `observation_incomplete` 또는 `comparison_error`로
실패하고, 일부 행을 일치로 출력하지 않는다. 이는 결함을 네 번째 정상 분류로
숨기는 대신 증거 부족을 명시하는 선택이다.

## 3. Q3 — T11이 보지 못하는 일시 효과

upper 기반 fsdiff는 spawn baseline과 종료 시점의 순변화만 보므로, run 중 생성한
파일을 다시 삭제하면 upper에 최종 entry나 whiteout이 남지 않을 수 있다.
`collector/fs_changed`의 `changes:[]`는 **최종 순변화가 없고 수집이 성공했다**는
뜻일 뿐, run 중 파일시스템 행위가 없었다는 증거가 아니다.

따라서 audit은 `changes:[]`를 보고-무관측이나 일치의 근거로 사용하지 않는다.
리포트 헤더에 `effect_observation: complete|incomplete`와 collector scan의
완전성·상한·lower 검증 결과를 표시한다. T11이 성공적으로 빈 diff를 반환한
경우에도 파일 행위 비교 표는 “비교 가능한 효과 0건”으로 두고, 행위 부재
주장을 만들지 않는다. `effect_observation=incomplete`이면 CLI는 성공적인
감사 결과 대신 구분 가능한 오류 종료를 반환한다.

이 공백은 upper scanner를 추측으로 확장해 메우지 않는다. syscall/exec 시계열로
일시 효과를 보완하는 FR-COL-04(eBPF 기반 exec 감사, v2)가 향후 증거원이며,
T12는 그 부재를 명시적으로 보고한다.

## 4. Q4 — 재계산 가능성과 저장 위치

리포트는 로그에 쓰지 않고 질의 시 계산한다. 입력은 session의 append-only
`gen.EventRecord` 전체이며, 다음을 순수하게 재계산한다.

* `subagent/tool_call`, `subagent/tool_result`, `subagent/done`에서 의도 행위와
  child span/call_id를 추출한다.
* `collector/fs_changed`와 `collector/egress`에서 효과 행위를 추출한다.
* event seq 순서를 검증하고, parent context와 usage는 기존 `logd.Replay`와
  동일한 로그 projection 규칙으로 산출한다.

audit 계산기는 `contracts`와 자체 value type만 사용한다. `collector`가 core를
import하거나 core가 collector를 import하는 경로를 만들지 않으며, collector의
내부 manifest/DB 타입도 참조하지 않는다. surface가 SQLite `Reader`로 이벤트를
한 번 읽어 audit analyzer에 넘기는 유일한 조립점이다. 따라서 계산 결과는
동일한 로그 snapshot에서 언제든 재생 가능하고, report를 append하는 별도 kind나
캐시가 진실을 바꾸지 않는다.

현재 contracts에는 `audit/report` kind가 없다. 이 단계에서는 새 kind를 만들지
않고, 계약 변경 없이 질의 결과를 stdout으로 산출한다. 향후 리포트를 durable
event로 보존해야 한다는 요구가 생기면 additive 변경이 아니라 report schema,
재계산·버전·민감정보 정책을 포함한 별도 SCP를 먼저 승인받는다.

## 5. Q5 — `hx audit` CLI 표면

`hx audit <session>`은 기존 `surfaces/hx`의 SQLite open/read 경로를 사용한다.
세션 로그를 한 번 읽고 검증·재계산을 완료한 뒤에만 stdout을 쓴다. 로그 손상,
복수 trace, 비교 불완전성은 stdout 0바이트, non-zero exit, 원인 진단은 stderr로
분리한다. 로그나 upper를 CLI가 수정하지 않는다.

기본 출력은 결정적 표다. 각 행에 `classification`, `span_id`, `parent_span_id`,
`path`, `reported_change`, `observed_change`, `intent_seq`, `effect_seq`,
`reason`을 포함하고, 끝에 세션 요약을 출력한다. 같은 입력은 실행 시각과 map
순회에 관계없이 byte-identical 결과를 낸다. 경로·raw payload·credential은
진단에 그대로 출력하지 않는다.

FR-AUD-03 질의를 위해 다음 선택적 필터를 둔다.

* `--span <id>`: 한 child span만 남기고, 해당 spawn의 직전 부모 context 요약과
  span별 분류를 출력한다.
* `--actor <name>`: usage와 비용을 actor별로 제한한다.
* `--cost`: session/span/agent 종류별 `usage_in`, `usage_out` 및 합계를 표에
  포함한다. 값은 `logd.Replay`의 checked usage projection과 동일하다.

필터가 매칭하는 span이 없으면 성공적인 빈 결과가 아니라 명시적 오류로 끝낸다.
FR-AUD-04의 임의 seq 부모 context는 `--at-seq <n>`으로 지정하며, 해당 seq
이전까지의 이벤트만 replay해 모델 가시 메시지를 재구성한다. 구현 시 기존
`hx replay --to`의 prefix semantics와 같은 경계(포함)를 사용한다.

## 6. Q6 — §8-3 인위적 불일치 시나리오

두 층으로 검증한다.

1. **결정적 report 단위 테스트:** T7 `NullAdapter`의 실제 정상 이벤트를
   subprocess로 재생하고, 동일 child span의 log snapshot에
   `subagent/tool_call`이 보고한 `echo` 행위와 대응하는 collector effect를
   한 건 제공한다. 별도의 collector event 한 건은 같은 span에서 보고 없이
   추가해 `observed_unreported`를 만들고, 반대로 tool_call만 남겨
   `reported_unobserved`를 만든다. 테스트는 세 분류, path 정규화, duplicate,
   cross-span, empty `changes`의 불완전성, span·cost·parent-context 질의를
   모두 검증한다. NullAdapter는 실제 구현인 척하지 않고 테스트 입력을 만드는
   **테스트 전용 Null 실행 파일**로 명시한다.
2. **Linux 관통 대조:** T11의 실제 FROM-scratch container testagent 위에서
   정상적으로 보고한 marker 변경과 보고하지 않은 hidden 변경을 함께 수행한다.
   동일 Linux `world-integration` gate에서 collector record와 tool_call을
   snapshot하고 audit analyzer에 넣어, `approved-marker.txt`는
   `matched`, hidden create/modify/delete는 `observed_unreported`인지
   확인한다. Podman/rootless/native-overlay가 없는 환경은 skip하지 않고
   실패한다.

단위 테스트는 보고/효과 로그를 손으로 조립하되 collector나 core 내부 타입을
가져오지 않는다. Linux 관통 테스트는 실제 T11 surface가 만든 durable log를
사용하므로 Fake backend가 완료 근거가 될 수 없다. NullAdapter 자체가 파일을
만지지 않는다는 점은 한계가 아니며, 거짓 보고를 표현하는 것은 로그 snapshot의
의도 행과 효과 행을 의도적으로 불일치시키는 테스트 fixture의 역할이다.

## 7. 경계·실패 모드·상한

* analyzer는 입력 event 수, 의도 행 수, 효과 행 수, serialized report bytes에
  상한을 둔다. 초과하면 부분 report를 출력하지 않고 오류로 끝낸다.
* raw payload는 원본성 검사용으로만 보유하고 report에는 path·credential·body를
  재출력하지 않는다.
* collector event가 actor `collector`가 아니거나 child span이 비어 있으면
  효과로 인정하지 않고 데이터 손상으로 실패한다.
* egress는 파일 대조와 별도 표면으로 유지한다. allow/deny decision과 deny
  reason, span, usage를 보존하며 domain 문자열만으로 파일 효과와 결합하지
  않는다.
* 리포트 생성 실패는 로그 append나 cleanup을 시도하지 않는다. 기존 append-only
  writer와 T11의 upper 보존/ACK 순서를 우회하지 않는다.

## 8. 구현 순서와 중지 조건

승인 뒤에도 다음을 분리한다.

1. contracts-only 입력 decoder와 canonical path/one-to-one matcher, 순수 단위
   테스트.
2. report value type와 deterministic renderer. 기존 contracts와 logd projection
   경계를 침범하지 않는지 boundarylint 회귀를 먼저 추가한다.
3. surfaces/hx가 Reader snapshot을 audit analyzer에 전달하고 `hx audit`를
   연결한다. span/cost/`--at-seq` 질의를 함께 검증한다.
4. T11 Linux gate에 positive marker와 hidden 변경의 분류를 추가하고, 동일 SHA로
   5회 연속 green을 확인한다.

다음이면 코드를 우회하지 않고 `BLOCKED.md`에 적는다.

* 동일 child span에서 canonical path를 안전하게 정규화할 수 없음.
* collector의 상대 경로와 tool_call의 의도 경로가 반복적으로 의미적으로
  충돌하여 one-to-one 결합이 불가능함.
* lower 재검사/scan 불완전성을 감지하지 못해 `changes:[]`를 행위 부재로
  오인하게 됨.
* audit 계산을 위해 collector-core 수평 import 또는 contracts 변경이 필수임.
* Linux 관통에서 보고 marker와 hidden 변경의 실제 span/seq 귀속이 설계와 다름.

## 9. 승인 요청

| # | 항목 | 성격 |
|---|---|---|
| 1 | Q1 결합 키: child span + canonical relative POSIX path + call_id/seq one-to-one, 정규화 실패는 일치 아님 | 대조 의미론 |
| 2 | Q2 세 분류와 ambiguous/incomplete의 fail-closed 처리 | FR-AUD-02 해석 |
| 3 | Q3 `changes:[]`를 행위 부재로 사용하지 않고 observation completeness를 별도 표시 | T11 한계 계승 |
| 4 | Q4 query-time 순수 계산, audit/report 신규 contract kind는 이번 범위에서 만들지 않음 | 재계산·경계 |
| 5 | Q5 `hx audit` 결정적 stdout 표, stderr 오류 분리, span/cost/`--at-seq` 질의 | FR-AUD-03/04·FR-CLI-04 |
| 6 | Q6 NullAdapter 기반 단위 불일치 + 실제 T11 Linux container 대조의 2층 검증 | §8-3 완료 기준 |
| 7 | T11 Linux run `33096358708`을 Q1 선행 실측 근거로 사용하고, T12 구현 시 별도 5회 관통 run을 요구 | 실행 증거 정책 |

승인 전에는 audit analyzer, CLI, contracts, integration workflow를 구현하지
않는다.
