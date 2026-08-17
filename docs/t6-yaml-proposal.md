# T6 사전 제안 — YAML 라이브러리 선정 (FR-POL-01 프로파일 파서)

상태: **승인됨** (2026-08-17 [H] 리뷰, 조건부) — goccy/go-yaml v1.19.2 도입 승인.
리뷰 정정·조건은 본문에 반영됨.
FR-POL-01은 정책 프로파일을 선언적 YAML로 표현할 것을 MUST로 요구한다 —
YAML 파서의 자체 구현은 규격 복잡도(anchor, 다중 문서, 타입 태그)상
합리적이지 않아 외부 라이브러리가 필요하다. 수치는 2026-08-17 GitHub API 조회.

## 후보

| | github.com/goccy/go-yaml | gopkg.in/yaml.v3 (go-yaml/yaml) | sigs.k8s.io/yaml |
|---|---|---|---|
| 최신 버전 | **v1.19.2** (2026-01-08) | v3.0.5 (유지보수 포크 기준) | v1.6.x |
| 상태 | 활성 (push 2026-04) | 구 저장소는 아카이브(2025-04)됐으나 **YAML.org 유지보수 포크(github.com/yaml/go-yaml)가 보안 수정 제공** — 현재 v3.0.5, v4 개발 중 ([H] 리뷰 정정) | 활성 (k8s) |
| 라이선스 | MIT | 저장소 라이선스 표기 비표준(NOASSERTION) | Apache-2.0 |
| 의존 | **모듈 의존성 0개** ([H] 리뷰 확인·정정) | 0 | yaml 계열 wrapper (JSON 경유 변환) |
| strict 모드 | `yaml.Strict()` — 미지 필드 거부 | KnownFields | JSON 태그 기반 |

## 추천안

**github.com/goccy/go-yaml v1.19.2** (MIT).

- 탈락: yaml.v3 — 구 저장소 아카이브. 유지보수 포크가 보안 수정을 제공하나
  ([H] 리뷰 정정) 전환기 상태(v4 개발 중)이고, goccy가 strict 모드·활성
  유지보수·의존 0으로 우위. sigs.k8s.io/yaml — 내부적으로 yaml 계열에
  재의존하는 wrapper라 공급망만 늘어난다.
- import 지점: `core/policy` 한 곳으로 한정 — boundarylint externalRestrictions에
  `github.com/goccy/go-yaml → core/policy` 추가(승인 의존성 3호, 기존 방식 동일).
- 파서 구현 조건 ([H] 승인 조건, 전부 이행):
  - strict 모드는 미지 필드·중복 키만 거부 — required·enum·비음수는 별도 검증
    (승인 모드 명시 필수 + manual|auto, 예산 3축 명시·비음수, fs 경로
    비어 있지 않은 절대경로·정규화)
  - **다중 문서 주의**: UnmarshalWithOptions(…, yaml.Strict())는 첫 문서만
    읽고 성공한다([H] 실증) — Decoder로 읽은 뒤 두 번째 Decode가 io.EOF인지
    반드시 검사 (구현·테스트 반영)
  - import는 core/policy 한 곳 — boundarylint externalRestrictions로 강제
- 공급망: 모듈 의존성 0개, go.sum 고정 완료.

구현: `core/policy/parse.go` + 위반 프로파일 거부 테스트 12종 (T6 브랜치).
