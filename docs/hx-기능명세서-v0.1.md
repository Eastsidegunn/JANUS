# HX 기능명세서 (v0.1 초안)

멀티 에이전트 실행 기판 — 이질적인 여러 에이전트를, 신뢰하지 않고도 책임 있게 부릴 수 있는 실행 환경.

본 문서는 기능 요구사항을 정의한다. 일정·마일스톤·조직 관련 내용은 범위 밖이다.

---

## 1. 목적과 범위

### 1.1 목적

외부 코딩 에이전트(Claude Code, Codex, pi, OpenClaw 등)와 사내 에이전트를 하나의 세션 안에서 오케스트레이션하되, 모든 행위가 (1) 단일 trace로 귀속되고, (2) 프로세스 경계에서 격리되며, (3) 사후에 재구성·검증 가능한 시스템을 제공한다.

### 1.2 범위 내

세션 로그와 리플레이, 에이전트 루프와 훅, 정책 평가·집행, 서브에이전트 어댑터, 하위 에이전트 네이티브 확장 패스스루(FR-EXT), 샌드박스 실행, 효과 평면 수집, 의도/효과 대조, CLI 표면, OTel 연동.

### 1.3 범위 외 (비목표)

자체 웹 UI, 정책 규칙 언어(DSL), HX 자체의 플러그인 마켓플레이스 운영(단, 하위 에이전트가 자기 생태계의 확장을 가져와 쓰는 것은 범위 내다 — FR-EXT 참조), 분산 컨트롤 플레인, 에이전트 루프 자체의 교체 가능성, 자체 LLM 추론.

---

## 2. 용어

| 용어 | 정의 |
|---|---|
| 세션 | 하나의 root trace에 대응하는 작업 단위. 로그 파일 하나와 1:1 |
| trace / span | OpenTelemetry 의미. 부모 세션 = root trace, 서브에이전트 실행 = child span |
| turn | 입력 하나가 소진될 때까지의 실행 단위. 0개 이상의 step으로 구성 |
| step | 모델 요청 1회 + 그에 따른 툴 실행들 |
| seam | 교체 가능하도록 인터페이스로 고정된 경계. 총 6개(모델, 툴, 실행 세계, 영속화, 서브에이전트, UI) |
| 의도 평면 | 에이전트/어댑터가 자발적으로 보고한 이벤트의 집합 |
| 효과 평면 | 샌드박스 경계에서 에이전트 협조 없이 강제 수집된 관측의 집합 |
| 관측 가능 에이전트 | 내부 툴 콜 수준의 중간 이벤트를 스트리밍하는 서브에이전트 |
| 불투명 에이전트 | spawn/done 수준의 이벤트만 제공하는 서브에이전트 |
| 프로파일 | 샌드박스·예산·승인 규칙을 선언한 정책 데이터 단위 |

요구사항 표기는 RFC 2119를 따른다: MUST(필수) / SHOULD(권장) / MAY(선택).

---

## 3. 시스템 개요

### 3.1 정적 구조 (4층, 단방향 의존)

```
contracts  →  core  →  seams  →  surfaces
                └──────(별도 경로)──────┐
                                   collector
```

- 층 0 `contracts`: 이벤트 스키마, 어댑터 와이어 프로토콜, 정책 타입. 의존성 없음.
- 층 1 `core`: 세션 로그, 루프, 정책 엔진. contracts만 의존. 교체 불가.
- 층 2 `seams`: 6개 seam의 구현체들. seam 간 수평 import 금지.
- 층 3 `surfaces`: CLI, 서버. 조립 지점.
- `collector`: 효과 평면. core와 코드 경로를 공유하지 않으며 span_id로만 조인.

### 3.2 런타임 구조

컨트롤 플레인(루프+정책, 로그 writer, 구독자) / 실행 플레인(샌드박스 안의 서브에이전트들) / 수집 경로(collector). 로그 진입점은 writer 하나뿐이다.

---

## 4. 기능 요구사항

### FR-LOG — 세션 로그

| ID | 요구사항 |
|---|---|
| FR-LOG-01 | 세션당 하나의 append-only 이벤트 로그를 유지해야 한다(MUST). UPDATE/DELETE는 스키마 마이그레이션 외에 금지되며 저장소 수준(트리거 등)에서 물리적으로 차단해야 한다(MUST). |
| FR-LOG-02 | 로그 쓰기는 단일 writer를 경유해야 하며(MUST), writer가 발급한 seq가 세션 내 이벤트의 전순서를 정의한다. |
| FR-LOG-03 | 모델 요청에 포함되는 모든 내용은 로그로부터 재구성 가능해야 한다(MUST, "model-visible means logged"). 런타임은 이 불변식을 assert해야 한다(SHOULD). |
| FR-LOG-04 | 모델 히스토리, 비용 집계, UI 상태 등 모든 파생 상태는 로그의 프로젝션이어야 하며(MUST), 캐시된 프로젝션은 언제든 로그에서 재계산 가능해야 한다(MUST). |
| FR-LOG-05 | 임의 seq 지점에서 세션을 포크할 수 있어야 한다(MUST). 포크된 세션은 새 trace_id를 갖되 원본 참조를 보존한다. |
| FR-LOG-06 | 세션 리플레이는 결정론적이어야 한다(MUST): 동일 이벤트 시퀀스의 재생은 동일 파생 상태를 산출한다. 이 성질은 속성 기반 테스트로 CI에서 검증되어야 한다(MUST). |
| FR-LOG-07 | 정규화 이벤트에는 원본 페이로드(raw)를 함께 보존해야 한다(MUST). raw는 압축 저장할 수 있다(MAY). |
| FR-LOG-08 | 로그 기록 전 redaction 패스를 적용해야 한다(MUST): 알려진 자격증명 패턴(토큰, 키)의 마스킹. redaction 규칙은 설정으로 확장 가능해야 한다(SHOULD). |
| FR-LOG-09 | 이벤트 폭주 시 writer는 백프레셔를 발생시켜야 하며(MUST), 어댑터 스트림 일시정지로 전파되어야 한다. 이벤트 유실은 허용되지 않는다. |
| FR-LOG-10 | 부모 컨텍스트에는 서브에이전트의 최종 결과·요약만 진입한다(MUST). 자식의 중간 이벤트 전체는 자식 span에 기록되되 부모 모델 히스토리에는 포함되지 않는다. |

### FR-LOOP — 에이전트 루프와 훅

| ID | 요구사항 |
|---|---|
| FR-LOOP-01 | turn/step 상태 머신은 코어에 고정되며 교체 불가하다(MUST). |
| FR-LOOP-02 | 훅 지점은 `pre_step`, `pre_tool`, `post_tool`, `turn_stopping` 4개로 고정한다(MUST). |
| FR-LOOP-03 | 각 훅은 독립 호출되며 판정 `continue | rewrite(x) | reject(reason)`을 반환한다(MUST). 미들웨어식 next() 체이닝은 제공하지 않는다. |
| FR-LOOP-04 | 다중 훅의 판정 충돌은 고정 규칙으로 해소한다(MUST): reject > rewrite > continue. rewrite가 복수이면 등록 순서대로 적용. |
| FR-LOOP-05 | reject된 첫 step은 step 없는 durable turn으로 로그에 남아야 한다(MUST) — 시도 자체가 기록 대상이다. |
| FR-LOOP-06 | turn/step 경계, 훅 판정, 판정 사유는 모두 세션 이벤트로 기록되어야 한다(MUST). |

### FR-POL — 정책

| ID | 요구사항 |
|---|---|
| FR-POL-01 | 정책은 선언적 프로파일(YAML)로 표현한다(MUST). v0.1 필드: fs 스코프, egress allow 리스트, 예산(토큰/시간/spawn 깊이), 승인 모드. |
| FR-POL-02 | 정책 평가는 순수 함수여야 한다(MUST): (프로파일, spawn 요청) → 거부 또는 샌드박스 설정. |
| FR-POL-03 | 프로파일 병합은 강화만 허용한다(MUST): allow 리스트는 교집합, 예산은 최솟값으로만 결합된다. 어떤 오버레이도 상위 프로파일보다 넓은 권한을 만들 수 없다. |
| FR-POL-04 | 정책 집행은 샌드박스 환경 설정으로 구워져야 한다(MUST). 실행 중 컨트롤 플레인이 중단되어도 제약은 유지된다. |
| FR-POL-05 | 서브에이전트의 승인 요청은 `subagent/approval_request` 이벤트로 승격되어 부모 정책 레이어가 판정한다(MUST). 자동 승인 모드는 프로파일에서 명시적으로만 켤 수 있다(MUST). |
| FR-POL-06 | 예산 초과 시 해당 서브에이전트는 stop되어야 하며(MUST), 사유가 이벤트로 기록된다. |
| FR-POL-07 | 불투명 에이전트에는 관측 가능 에이전트보다 좁은 기본 프로파일이 적용되어야 한다(SHOULD) — 신뢰도와 격리 강도의 반비례. |

### FR-ADP — 서브에이전트 어댑터

| ID | 요구사항 |
|---|---|
| FR-ADP-01 | 어댑터는 stdin/stdout으로 NDJSON 와이어 프로토콜(§5.2)을 말하는 독립 실행 파일이다(MUST). 구현 언어는 제약하지 않는다. |
| FR-ADP-02 | 어댑터 인터페이스는 spawn / send / events / stop / approval의 최소 계약으로 한정한다(MUST). 대상 도구별 고유 기능은 어댑터 설정으로만 노출한다. |
| FR-ADP-03 | 어댑터는 대상 에이전트의 네이티브 이벤트를 정규화 이벤트 어휘(§5.1)로 변환해야 하며(MUST), 최소 `subagent/ready`와 `subagent/done`을 방출해야 한다(MUST). 중간 이벤트는 가능한 만큼 방출한다(SHOULD). |
| FR-ADP-04 | 정규화 이벤트에는 원본 페이로드를 raw 필드로 첨부해야 한다(MUST). |
| FR-ADP-05 | 각 어댑터는 대상 도구의 녹화 출력(fixture)에 대한 스냅샷 테스트를 보유해야 한다(MUST). 대상 도구의 출력 포맷 변경은 CI에서 검출되어야 한다. |
| FR-ADP-06 | 어댑터는 자신을 관측 가능/불투명 등급으로 선언해야 한다(MUST). |
| FR-ADP-07 | 토큰 사용량(usage)은 옵셔널 필드로 보고한다(SHOULD). 미보고 시 코어는 시간·요금 기반 추정으로 폴백한다(SHOULD). |
| FR-ADP-08 | spawn 깊이 제한과 잔여 예산은 환경변수로 자식에 전파되어야 하며(MUST), 어댑터는 깊이 초과 spawn을 거부해야 한다(MUST). |
| FR-ADP-09 | v0.1 어댑터 대상: Claude Code(stream-json), Codex(비대화형 실행), pi(SDK), OpenClaw(게이트웨이 API), 사내 에이전트(HTTP/gRPC 참조 구현). |
| FR-ADP-10 | 어댑터 프로세스는 호스트 측에서 실행되고 에이전트 본체만 샌드박스 안에서 실행된다(MUST) — 에이전트는 어댑터를 변조할 수 없어야 한다. |

### FR-EXT — 하위 에이전트 확장 패스스루

| ID | 요구사항 |
|---|---|
| FR-EXT-01 | 어댑터는 대상 에이전트의 네이티브 확장(플러그인, 스킬, MCP 서버 등) 목록을 spawn 설정으로 받을 수 있어야 한다(대상이 확장을 지원하는 경우 MUST). HX는 확장의 저장소·큐레이션을 운영하지 않는다 — 출처는 각 에이전트 생태계다. |
| FR-EXT-02 | 확장 선언은 버전 고정과 무결성 해시를 포함해야 한다(MUST). 미고정 선언(예: latest)은 정책으로 거부 가능하며 기본값은 거부다(SHOULD). |
| FR-EXT-03 | 확장 설치는 실행 전 프로비저닝 단계에서 수행한다(MUST). 프로비저닝은 레지스트리 도메인만 허용하는 전용 egress 프로파일로 실행되며, 해당 도메인은 실행 단계 프로파일에 자동 승계되지 않는다(MUST). |
| FR-EXT-04 | 설치된 확장 세트(이름, 버전, 해시, 출처)와 프로비저닝 결과물의 해시는 spawn 이벤트 메타데이터에 기록되어야 한다(MUST) — 리플레이·포크 시 동일 확장 환경의 재현 근거. |
| FR-EXT-05 | 정책 프로파일은 허용 확장·허용 레지스트리 allowlist를 가질 수 있으며(MAY), 병합은 교집합으로만 결합된다(MUST, FR-POL-03과 일관). |
| FR-EXT-06 | 실행 중 네트워크가 필요한 확장(MCP 서버 등)은 필요한 egress 도메인을 선언에 명시해야 하며(MUST), 정책 평가를 통과한 도메인만 실행 프로파일에 편입된다(MUST). |
| FR-EXT-07 | 확장 코드는 컨테이너 내 비신뢰 코드로 간주하며(MUST, NFR-06의 계), 효과 평면 관측 대상에 포함된다. HX는 확장의 안전성을 심사하지 않는다. |
| FR-EXT-08 | 확장 아티팩트는 콘텐츠 주소(해시) 기반 로컬 캐시를 지원한다(SHOULD) — 반복 spawn의 프로비저닝 비용 절감. |

### FR-SBX — 샌드박스 / 실행 세계

| ID | 요구사항 |
|---|---|
| FR-SBX-01 | 서브에이전트는 OCI 컨테이너 안에서 실행되어야 한다(MUST). rootless 실행을 기본으로 한다(SHOULD). |
| FR-SBX-02 | 워크스페이스는 지정 경로만 마운트한다(MUST). overlayfs 기반으로 원본은 lower, 변경은 upper에 격리한다(MUST). |
| FR-SBX-03 | 컨테이너 네트워크는 기본 차단이며(MUST), 프로파일의 allow 리스트에 있는 도메인만 강제 프록시를 경유해 허용한다(MUST). |
| FR-SBX-04 | 자격증명은 스코프를 좁힌 단기 토큰으로 spawn 시 주입한다(MUST). 장기 자격증명의 컨테이너 반입은 금지한다(MUST). |
| FR-SBX-05 | 실행 세계 seam(fs+subprocess+sandbox)은 하나의 인터페이스로 통합한다(MUST). 백엔드 교체(local → 원격 microVM)는 소비자 코드 변경 없이 가능해야 한다(MUST). |
| FR-SBX-06 | 샌드박스 프로파일 ID, 마운트 경로, 이미지 해시 등 실행 환경 메타데이터는 spawn 이벤트에 기록되어야 한다(MUST). |

### FR-COL — 수집기 (효과 평면)

| ID | 요구사항 |
|---|---|
| FR-COL-01 | 수집기는 에이전트의 협조 없이 동작해야 한다(MUST). 에이전트가 비정상 종료해도 수집된 관측은 보존된다. |
| FR-COL-02 | fs diff: spawn 시 워크스페이스 상태를 기준으로, done 시 변경 파일 목록(경로, 해시, 변경 유형)을 `collector/fs_changed` 이벤트로 방출해야 한다(MUST). overlayfs upper 디렉토리를 diff 소스로 사용한다(SHOULD). |
| FR-COL-03 | egress: 프록시를 통과한 요청(도메인, 메서드, 크기, 시각)을 `collector/egress` 이벤트로 방출해야 한다(MUST). 페이로드 본문은 기록하지 않는다(MUST NOT) — 메타데이터만. |
| FR-COL-04 | exec 감사(eBPF 기반 syscall 관측)는 v2 범위로 한다(MAY). 인터페이스 자리만 v0.1에 확보한다. |
| FR-COL-05 | 모든 수집 이벤트는 해당 서브에이전트의 span_id로 귀속되어야 한다(MUST). |
| FR-COL-06 | 수집 이벤트도 writer를 경유해 동일 로그에 기록된다(MUST). actor 필드로 의도 평면 이벤트와 구분한다. |

### FR-AUD — 대조와 감사

| ID | 요구사항 |
|---|---|
| FR-AUD-01 | 세션 단위 대조 리포트를 생성할 수 있어야 한다(MUST): 서브에이전트별 "보고된 행위(의도 평면) vs 관측된 행위(효과 평면)"의 표와 불일치 항목. |
| FR-AUD-02 | 불일치 유형을 분류해야 한다(SHOULD): 보고됐으나 관측 없음(환각/거짓 보고 의심), 관측됐으나 보고 없음(은폐/누락 의심), 일치. |
| FR-AUD-03 | 다음 질의가 가능해야 한다(MUST): 특정 파일 변경의 span 귀속, 특정 spawn의 직전 부모 컨텍스트 조회, 세션·span별 비용 집계, 에이전트 종류별 비용 분해. |
| FR-AUD-04 | 임의 seq 지점의 부모 모델 가시 컨텍스트를 재구성할 수 있어야 한다(MUST) — "그 시점에 부모가 무엇을 알았는가". |

### FR-CLI — 표면

| ID | 요구사항 |
|---|---|
| FR-CLI-01 | `hx run <instruction>`: 새 세션 시작(MUST). |
| FR-CLI-02 | `hx replay <session>`: 세션 재생. `--to <seq>` 지원(MUST). |
| FR-CLI-03 | `hx fork <session>@<seq>`: 지점 포크(MUST). |
| FR-CLI-04 | `hx audit <session>`: FR-AUD-01의 대조 리포트 출력(MUST). |
| FR-CLI-05 | `hx dump-config`: 프로파일 병합 후 실제 부팅되는 최종 설정 트리 출력(MUST). |
| FR-CLI-06 | 종료 코드와 stderr/stdout 분리는 파이프라인 사용을 전제로 설계한다(SHOULD): 이벤트는 stdout NDJSON, 진단은 stderr. |

### FR-OBS — 관측 연동

| ID | 요구사항 |
|---|---|
| FR-OBS-01 | 세션 로그를 OpenTelemetry trace/span으로 export할 수 있어야 한다(MUST). trace_id/span_id는 OTel 규격과 호환되게 발급한다(MUST). |
| FR-OBS-02 | span 속성에 최소 포함(MUST): 어댑터 종류·버전, 프로파일 ID, usage, 종료 상태. |
| FR-OBS-03 | 자체 시각화 UI는 제공하지 않는다(MUST NOT, v0.1) — 기존 OTel 생태계 뷰어 사용을 전제로 한다. |

---

## 5. 데이터 모델

### 5.1 이벤트 스키마

```sql
CREATE TABLE events (
  seq            INTEGER PRIMARY KEY,  -- writer 발급, 세션 내 전순서
  ts             INTEGER NOT NULL,     -- unix ms
  trace_id       TEXT NOT NULL,
  span_id        TEXT NOT NULL,
  parent_span_id TEXT,
  kind           TEXT NOT NULL,
  actor          TEXT NOT NULL,        -- parent | subagent:{adapter}:{n} | collector
  payload        TEXT NOT NULL,        -- 정규화 JSON
  raw            BLOB,                 -- 원본 passthrough(압축), 옵셔널
  usage_in       INTEGER,
  usage_out      INTEGER
);
```

이벤트 종류(kind) 어휘, v0.1:

| 도메인 | kind |
|---|---|
| 세션 | `session/start`, `session/fork`, `session/end` |
| 턴/스텝 | `turn/start`, `turn/end`, `step/start`, `step/end` |
| 대화 | `user/message`, `assistant/chunk`, `assistant/message` |
| 툴 | `tool/call`, `tool/result` |
| 훅 | `hook/verdict` (지점, 판정, 사유) |
| 서브에이전트 | `subagent/spawn`, `subagent/ready`, `subagent/message`, `subagent/tool_call`, `subagent/tool_result`, `subagent/approval_request`, `subagent/usage`, `subagent/done` |
| 수집기 | `collector/fs_changed`, `collector/egress` |
| 정책 | `policy/decision` (허용/거부, 적용 프로파일) |

`subagent/spawn` payload는 `world_backend` 판별자로 폐쇄한다
(SCP-T10-001, 2026-08-21 [H] 승인). 공통 필드는 `adapter`, `instruction`,
`depth`, `budget`, `world_backend`다. `world_backend:"none"`은 기존 null adapter와
테스트 경로를 표현하며 sandbox metadata를 허용하지 않는다.
`world_backend:"local-podman"`은 `profile_id`, tag가 아닌
`sha256:<64 lowercase hex>` image digest, workspace overlay `mounts`를 추가로
필수화한다. 두 분기 모두 미지 필드를 거부한다. schema가 `none`을 허용하는 것은
production 권한이 아니며, production surface는 FR-SBX-01에 따라 이를 거부한다.

스키마 확장 규칙: 새 모델 가시 입력은 반드시 새 kind 추가로 처리한다(FR-LOG-03의 계). kind의 제거·의미 변경은 메이저 버전에서만 허용한다.

### 5.2 어댑터 와이어 프로토콜 (NDJSON over stdio)

모든 메시지는 한 줄 JSON이며 `v`(프로토콜 버전) 필드를 포함한다(MUST).

코어 → 어댑터 (stdin):

| cmd | payload |
|---|---|
| `task` | instruction, workspace 경로, 예산, depth, extensions(옵셔널 — 이름/버전/무결성 해시/출처/필요 egress 도메인의 배열, FR-EXT) |
| `message` | 추가 입력 텍스트 |
| `stop` | reason (`user`, `budget_exceeded`, `policy`, `parent_done`) |
| `approval_response` | request_id, decision(`allow`/`deny`), reason(deny 시 필수) — `subagent/approval_request`에 대한 부모 정책 레이어의 판정 반환 (FR-POL-05) |

어댑터 → 코어 (stdout):

| kind | 필수 여부 |
|---|---|
| `subagent/ready` | MUST |
| `subagent/message` | SHOULD |
| `subagent/tool_call` / `tool_result` | SHOULD (관측 가능 등급이면 MUST) |
| `subagent/approval_request` | 해당 시 MUST |
| `subagent/usage` | SHOULD |
| `subagent/done` | MUST — status(`ok`,`error`,`stopped`), result 포함 |

프로토콜 준수의 검증 기준은 contracts 저장소의 JSON Schema와 골든 픽스처다. 픽스처를 통과하지 못하는 어댑터는 등록될 수 없다(MUST).

---

## 6. 비기능 요구사항

| ID | 요구사항 |
|---|---|
| NFR-01 | 결정론: 동일 로그의 리플레이는 동일 파생 상태를 산출한다(FR-LOG-06과 동일 불변식, 시스템 수준 재명시). |
| NFR-02 | 내구성: 이벤트는 writer의 커밋 시점에 fsync 수준으로 내구적이어야 한다(MUST). 크래시 후 재기동 시 마지막 커밋 이벤트까지 복원된다. |
| NFR-03 | 저장소: v0.1은 세션당 SQLite(WAL) 파일 하나. store seam 뒤에서 Postgres로 교체 가능해야 한다(MUST). |
| NFR-04 | 이식성: 컨트롤 플레인·collector는 단일 정적 바이너리로 배포 가능해야 한다(SHOULD). |
| NFR-05 | 오버헤드: 로그 경유와 컨테이너 격리로 인한 오버헤드는 수용한다 — 최단 경로 성능은 본 제품의 최적화 목표가 아니다. 단, writer는 단일 세션 기준 초당 수천 이벤트를 유실 없이 처리해야 한다(SHOULD). |
| NFR-06 | 보안: 신뢰 경계는 컨테이너 벽과 일치한다. 컨테이너 내부 코드는 전부 비신뢰로 간주한다(MUST). |
| NFR-07 | 보존·접근: 로그의 보존 기간과 접근 권한은 배포 설정으로 정의 가능해야 한다(SHOULD). raw 필드는 별도 접근 등급을 가질 수 있다(MAY). |

---

## 7. 시스템 불변식 (요약)

1. 사실은 불변, 해석은 파생 — 로그 밖의 진실 없음.
2. 로그 진입점은 writer 하나 — seq가 곧 전순서.
3. 모델이 본 것은 로그에 있다 — 재구성 불가능한 모델 입력 금지.
4. 정책 병합은 강화만 — 어떤 조합도 권한을 넓힐 수 없음.
5. 효과 평면은 협조 무관 — 에이전트가 죽어도, 거짓말해도 관측은 남는다.
6. 의존은 아래로만 — contracts ← core ← seams ← surfaces, seam 간 수평 금지.
7. 부모 컨텍스트에는 자식의 결과만 — 자식 trace는 자식 span에.

---

## 8. 수용 기준 (릴리스 게이트)

v0.1은 다음이 모두 참일 때 완료로 본다.

1. 리플레이 결정론 속성 테스트가 CI에서 통과한다 (FR-LOG-06).
2. Claude Code와 Codex 어댑터가 골든 픽스처 테스트를 통과하고, 실 세션에서 자식 툴 콜이 child span으로 기록된다 (FR-ADP-05, FR-ADP-09).
3. 임의 세션에 대해 `hx audit`이 fs diff 기반 대조 리포트를 산출하고, 인위적 불일치(에이전트가 보고하지 않은 파일 변경)를 검출한다 (FR-AUD-01/02).
4. egress allow 리스트 밖의 도메인 접근이 실제로 차단되고 `collector/egress`에 시도가 기록된다 (FR-SBX-03, FR-COL-03).
5. 프로파일 오버레이로 권한이 넓어지는 조합이 존재하지 않음을 속성 테스트로 검증한다 (FR-POL-03).
6. `hx fork` 후 두 세션이 독립적으로 진행되며 원본 로그가 불변임이 확인된다 (FR-LOG-05).
7. OTel export된 trace가 표준 뷰어(Jaeger 등)에서 부모-자식 span 트리로 렌더링된다 (FR-OBS-01).
8. 확장 선언이 있는 spawn에서 프로비저닝 단계의 레지스트리 접근은 성공하고, 실행 단계의 동일 도메인 접근은 차단되며, 설치된 확장 세트가 spawn 이벤트에 기록된다 (FR-EXT-03/04).

---

## 부록 A. 미결 사항

- 사내 에이전트 어댑터의 참조 프로토콜(HTTP vs gRPC) 확정.
- raw 필드 압축 포맷과 보존 등급 정책.
- exec 감사(eBPF)의 v2 상세 명세.
- Postgres store 전환 트리거 기준(동시 세션 수, 중앙 조회 요구).
- redaction 규칙의 기본 세트 범위.
