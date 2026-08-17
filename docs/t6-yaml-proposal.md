# T6 사전 제안 — YAML 라이브러리 선정 (FR-POL-01 프로파일 파서)

상태: **승인 대기** (2026-08-17). 승인 전에는 의존성을 추가하지 않는다.
FR-POL-01은 정책 프로파일을 선언적 YAML로 표현할 것을 MUST로 요구한다 —
YAML 파서의 자체 구현은 규격 복잡도(anchor, 다중 문서, 타입 태그)상
합리적이지 않아 외부 라이브러리가 필요하다. 수치는 2026-08-17 GitHub API 조회.

## 후보

| | github.com/goccy/go-yaml | gopkg.in/yaml.v3 (go-yaml/yaml) | sigs.k8s.io/yaml |
|---|---|---|---|
| 최신 버전 | **v1.19.2** (2026-01-08) | v3.0.1 (2022) | v1.6.x |
| 상태 | 활성 (push 2026-04) | **아카이브됨 (2025-04)** — 더 이상 유지보수 없음 | 활성 (k8s) |
| 라이선스 | MIT | 저장소 라이선스 표기 비표준(NOASSERTION) | Apache-2.0 |
| 의존 | 소규모 | 0 | yaml 계열 wrapper (JSON 경유 변환) |
| strict 모드 | `yaml.Strict()` — 미지 필드 거부 | KnownFields | JSON 태그 기반 |

## 추천안

**github.com/goccy/go-yaml v1.19.2** (MIT).

- 탈락: yaml.v3 — 업스트림 아카이브(보안 패치 종료). sigs.k8s.io/yaml —
  내부적으로 yaml 계열에 재의존하는 wrapper라 공급망만 늘어난다.
- import 지점: `core/policy` 한 곳으로 한정 — boundarylint externalRestrictions에
  `github.com/goccy/go-yaml → core/policy` 추가(승인 의존성 3호, 기존 방식 동일).
- 파서 요구사항: strict 모드(미지 필드 거부 — fail-closed), 승인 모드는
  명시 필수(zero value로 auto가 되는 경로 금지), 음수 예산 거부.
- 공급망: goccy/go-yaml의 직접 의존 확인 후 go.sum 고정 (구현 시 기록).

승인되면 T6 브랜치에 `core/policy/parse.go` + 위반 프로파일 거부 테스트를 추가한다.
