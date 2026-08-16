# T1 사전 제안 — JSON Schema → Go codegen 도구 선정

상태: **승인 대기** (2026-08-16). 이 문서는 비교·제안만 담는다.
이 PR은 외부 의존성을 추가하지 않고 contracts/ 스키마·생성 코드도 구현하지 않는다.
아래 수치(버전, 라이선스, 최근 push, 의존 수)는 2026-08-16 GitHub API 조회 결과다.

---

## 0. 전제: 타입 생성과 런타임 검증은 별개 문제다

T1 완료 기준은 두 축이다: (a) §5.1 예시 이벤트가 codegen 산출 타입으로 파싱·검증되고,
(b) **스키마 위반 샘플은 거부**된다. (b)는 Go 타입만으로 달성할 수 없다 — Go의
`encoding/json`은 미지 필드(`additionalProperties: false` 위반), enum 밖의 값, 잘못된
format을 조용히 통과시킨다. 또한 §5.2는 "프로토콜 준수의 검증 기준은 contracts의
JSON Schema와 골든 픽스처"라고 명시하므로, 검증은 생성된 타입이 아니라 **스키마 문서
그 자체**로 수행되어야 한다(스키마가 진실이라는 불변식과 정합).

따라서 후보를 두 트랙으로 나눠 평가한다.

- **생성기(G)**: JSON Schema → Go struct. 빌드 타임 전용.
- **검증기(V)**: 스키마 문서로 인스턴스를 런타임 검증. 코어·어댑터 등록 게이트에서 사용.

## 1. 후보

| ID | 후보 | 최신 버전 | 라이선스 | 최근 push | 직접 의존 |
|---|---|---|---|---|---|
| G1 | 자체 제너레이터 (`tools/schemagen`, 표준 라이브러리만) | — | 저장소와 동일 | — | 0 |
| G2 | omissis/go-jsonschema (모듈 경로는 `github.com/atombender/go-jsonschema`) | v0.24.1 (2026-08-01) | MIT | 2026-08-14 | 8 (cobra, goccy/go-yaml, mergo, litter 등) |
| G3 | glideapps/quicktype | v26.0.0 (2026-07-20) | Apache-2.0 | 2026-08-10 | Node.js 생태계 전체 |
| — | a-h/generate | 릴리스 없음 | MIT | 2024-04 | (사실상 중단 — 후보 제외) |
| V1 | santhosh-tekuri/jsonschema/v6 | v6.0.3 (2026-08-06) | Apache-2.0 | 2026-08-06 | 1 (golang.org/x/text) + 테스트 전용 1 (dlclark/regexp2) |
| V2 | kaptinlin/jsonschema | v0.9.8 (2026-08-03) | MIT | 2026-08-03 | (pre-1.0) |
| V3 | xeipuuv/gojsonschema | v1.2.0 (**2019-10**) | API상 라이선스 미검출 | 2024-06 | — |
| V0 | 자체 검증기 구현 | — | — | — | 0 |

## 2. JSON Schema draft 및 키워드 지원 (§요건 3)

HX 스키마는 **draft 2020-12**로 작성하고, 사용 키워드를 아래 서브셋으로 제한할 것을
전제한다(스키마를 우리가 저작하므로 제한 가능):

`type, properties, required, additionalProperties, $defs, $ref(파일 내부만), enum,
const, oneOf(+판별 필드 const), items, format, minimum/maxLength 류 제약`

| | draft | $ref | enum | oneOf | required | additionalProperties | format |
|---|---|---|---|---|---|---|---|
| G1 | 위 서브셋만 수용, **서브셋 밖 키워드는 생성 실패(fail-closed)** | 파일 내부 | ○ | 판별 const 한정 | ○ | ○ | 주석/타입 매핑만 |
| G2 | draft-07 중심 | ○ | ○ | **✗ (README 체크리스트에서 anyOf/oneOf 미지원 확인)** | 부분(Unmarshal에 주입) | 부분 | 부분 |
| G3 | 다중 draft | ○ | ○ | ○(합집합 타입) | ○ | ✗(Go 출력) | ✗(Go 출력) |
| V1 | draft-04/06/07/2019-09/**2020-12** 전부 (bowtie 공식 compliance 배지) | ○ | ○ | ○ | ○ | ○ | assertion 옵션으로 강제 가능 |
| V2 | 2020-12 | ○ | ○ | ○ | ○ | ○ | ○ |
| V3 | draft-04/06/07 (2020-12 없음) | ○ | ○ | ○ | ○ | ○ | 부분 |

G2의 oneOf 미지원은 이 프로젝트에 치명적이다: §5.2 와이어 프로토콜은 `cmd`/`kind`로
판별되는 태그드 유니온이고, §5.1 이벤트도 kind별 payload 형태가 갈린다. 이를 oneOf
없이 표현하면 스키마 자체를 도구에 맞춰 뭉개야 한다 — 스키마가 진실이라는 원칙의 역전.

## 3. 타입 생성 vs 런타임 검증 지원 (§요건 4)

- G1/G3: 타입만 생성. 검증 없음(의도된 분리).
- G2: required 검사 일부를 `UnmarshalJSON`에 생성해 넣지만 완전한 스키마 검증이 아니다.
  "생성 코드에 반쯤 심긴 검증"은 스키마와 검증 로직의 이중 진실을 만든다.
- V1/V2: 스키마 문서를 컴파일해 인스턴스를 검증. 상세 오류 경로 제공.
- **결론**: 생성기는 형태만 담당하고, 거부(완료 기준 b)는 검증기가 embed된 스키마
  원문으로 수행한다.

## 4. 결정성·버전 고정·오프라인 재현성 (§요건 5)

- G1: 출력을 완전 통제 — 키 정렬 순회, 타임스탬프 미포함, `go/format` 통과, 생성
  결과를 커밋하고 골든 테스트로 고정. 도구 버전은 저장소 커밋 그 자체.
- G2: 대체로 결정적이나 출력 스타일이 도구 버전에 종속 — 마이너 업그레이드가 생성
  코드 전면 diff를 유발할 수 있고 우리가 통제할 수 없다.
- G3: Node 툴체인·npm 공급망 재현성 부담. CI에 Node 추가 필요.
- V1/V2: `go.mod`+`go.sum` 고정, GOPROXY 모듈 캐시로 오프라인 재현. (전면 오프라인이
  필요해지면 `go mod vendor` 채택 가능 — 이번엔 미채택, 필요 시 별도 결정.)

## 5. 유지보수·라이선스·공급망 (§요건 6)

§1 표 참조. 추가 판단:

- V3(xeipuuv)는 마지막 릴리스 2019년, 2020-12 미지원, open issue 144 — 탈락.
- V2(kaptinlin)는 활발하나 pre-1.0(v0.9.x)·성숙도 낮음 — contracts는 이 저장소에서
  가장 되돌리기 비싼 층이므로 검증기는 성숙한 쪽을 택한다.
- G2는 빌드 도구임에도 직접 의존 8개(CLI 프레임워크 포함)가 `go.sum`에 편입된다.
- V1은 직접 의존이 사실상 `golang.org/x/text` 하나 — 후보 중 최소 공급망.

## 6. 빌드 전용 의존성 격리 (§요건 7)

- G1: 저장소 내부 코드 — 격리 문제 자체가 없음.
- G2/G3 채택 시: Go 1.24+의 `go.mod` `tool` 지시자로 빌드 전용 고정 가능하나,
  `go.sum` 공급망 편입은 동일하다. G3는 Go 툴체인 밖(Node)이라 격리 더 어려움.
- V1: 런타임 의존이므로 격리 불가가 맞다. 대신 **import 지점을 contracts의 검증
  패키지 하나로 한정**한다. (외부 모듈 import 지점 제한은 현 boundarylint 범위 밖 —
  원하면 후속으로 허용 외부 의존 allowlist를 린터에 추가할 수 있다. 옵션으로만 기록.)

## 7. codegen drift 검출 (§요건 8)

```
make codegen   # tools/schemagen이 contracts/*.schema.json → contracts/gen/*.go 재생성
make ci        # lint → test → fixtures → codegen-drift
# codegen-drift: make codegen 후 `git diff --exit-code contracts/gen`
```

- 생성 파일 머리에 "생성물 — 손으로 수정 금지" 헤더와 `//go:generate` 마커.
- CI에서 재생성 결과가 커밋과 다르면 실패 — 스키마만 고치고 재생성을 잊거나,
  생성물을 손으로 고친 경우(CLAUDE.md 금지 행동) 모두 잡힌다.
- 생성기 자체의 골든 테스트: 고정 입력 스키마 → 기대 출력 소스 비교.

## 8. 스키마 진화·도구 교체 비용 (§요건 9)

- 스키마가 진실이므로 도구 교체 = 같은 스키마를 새 도구로 재생성 + 골든 diff 확인.
  §2의 키워드 서브셋 문서화가 교체 가능성의 담보다(서브셋 밖 기능에 기대지 않음).
- kind 추가는 additive(§5.1 확장 규칙)이므로 스키마 diff → `make codegen` →
  기존 골든 픽스처 불변 확인 흐름으로 처리된다.
- G1의 유지보수 비용은 서브셋 크기에 비례한다. 서브셋이 커질수록 자체 구현 부담이
  늘어나는 것이 G1의 본질적 리스크이며, 그 시점의 탈출구가 위 교체 경로다.

## 9. 결정 요청: contracts "의존성 없음"의 해석

§3.1은 "층 0 contracts: 의존성 없음"이라 한다. 본 제안은 이를 **내부 층 의존
없음**으로 해석한다(§3.1의 주제가 층 간 의존 방향이므로). 이 해석 하에:

- `contracts/gen/` (생성 타입): 표준 라이브러리만 — 외부 의존 0 유지.
- `contracts/validate/` (검증 헬퍼): 스키마를 `go:embed`하고 V1을 import하는 유일한 지점.

만약 "외부 의존 포함 전면 금지"로 해석한다면 검증 헬퍼를 core로 내려야 하지만,
§5.2가 "준수 기준은 contracts의 JSON Schema"라고 명시하므로 검증의 소유권은
contracts에 두는 전자가 명세와 더 정합한다. **리뷰에서 해석 확정을 요청한다.**

---

## 10. 추천안

**생성기 = G1 자체 제너레이터, 검증기 = V1 santhosh-tekuri/jsonschema/v6.**

### 탈락 사유

- **G2 (go-jsonschema v0.24.1)**: oneOf/anyOf 미지원 — 태그드 유니온인 §5.1/§5.2를
  표현할 수 없어 스키마를 도구에 맞춰 왜곡하게 된다. 직접 의존 8개의 공급망 부담.
  생성 출력 스타일에 대한 통제권 부재.
- **G3 (quicktype v26)**: Go 출력에 검증 없음, Node 툴체인이 CI·재현성에 편입되는
  비용이 단일 용도 대비 과대.
- **a-h/generate**: 2024-04 이후 활동 없음, 릴리스 없음 — 후보 자격 미달.
- **V2 (kaptinlin v0.9.8)**: 활발하지만 pre-1.0. contracts 층의 게이트로 쓰기엔
  성숙도 부족.
- **V3 (xeipuuv v1.2.0)**: 2019년 이후 릴리스 없음, draft 2020-12 미지원 — 탈락.
- **V0 (자체 검증기)**: JSON Schema 검증 규격은 자체 구현하기에 방대하고, 검증
  정합성 자체가 T1 완료 기준이자 어댑터 등록 게이트(§5.2)다. 여기서의 자체 구현은
  절약이 아니라 리스크다. (반면 생성기는 우리가 저작한 서브셋만 다루므로 자체 구현이
  합리적 — 두 트랙의 결론이 갈리는 이유.)

### 고정 버전·신규 의존성 (승인 대상)

| 모듈 | 버전 | 라이선스 | 성격 |
|---|---|---|---|
| `github.com/santhosh-tekuri/jsonschema/v6` | **v6.0.3** | Apache-2.0 | 직접, 런타임 (import 지점: `contracts/validate` 한정) |
| `golang.org/x/text` | v6.0.3이 요구하는 버전(go.sum 고정) | BSD-3 | 간접 |

이외 외부 의존성 없음. 생성기는 표준 라이브러리만 사용.

### Makefile·CI 흐름 (구현 PR에서 반영 예정)

- Makefile: `codegen` 타깃 신설, `ci`에 `codegen-drift`(재생성 후
  `git diff --exit-code contracts/gen`) 추가.
- `.github/workflows/ci.yml`: `make ci`가 그대로 drift까지 커버 — 워크플로 변경 불요.

### 변경 예정 파일 (구현 PR 범위)

| 파일 | 내용 |
|---|---|
| `contracts/events.schema.json` | §5.1 이벤트 스키마 (kind 어휘 전체) — [H] 리뷰 대상 |
| `contracts/wire.schema.json` | §5.2 와이어 프로토콜 스키마 — [H] 리뷰 대상 |
| `contracts/gen/*.go` | 생성 타입 (커밋되는 생성물, 수정 금지 헤더) |
| `contracts/validate/` | `go:embed` 스키마 + V1 기반 검증 헬퍼, 위반 샘플 거부 테스트 |
| `tools/schemagen/` | 자체 제너레이터 + 골든 테스트 (§2 서브셋, fail-closed) |
| `Makefile` | `codegen`, `codegen-drift` |
| `go.mod` / `go.sum` | V1 v6.0.3 추가 |
| `docs/traceability.md` | T1 행 추가 |

승인 후 구현 순서: 스키마 저작([H] 리뷰) → schemagen → validate → drift 게이트.
