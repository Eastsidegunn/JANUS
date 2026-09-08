# 멀티레포 랜드스케이프 — HX · Hi-Fi Pi · Rhizome · JARVIS

이 문서는 네 프로젝트로 하나의 "목표를 맡기고 감독하는 에이전트 운영
플랫폼"을 만드는 구조 논의의 핸드오프다. **다른 세션이 명세 논의를 이어갈 수
있도록** 결정된 것과 열린 것을 구분해 기록한다. 작성 2026-09-08.

이 문서는 JANUS(HX) 레포에 있지만 네 프로젝트를 가로지르는 메타 문서다.
장기적으로는 공유 계약 레포로 옮기는 게 맞다(아래 열린 질문 참조).

---

## 0. 지금 존재하는 것 vs 제안일 뿐인 것 — 먼저 읽어라

| 프로젝트 | 상태 | 위치 |
|---|---|---|
| **HX / JANUS** | **존재함.** T0–T16 완료, v0.1 기판 (문서화된 잔여 있음) | `~/Gunnsplayground/JANUS` (Go) |
| **Hi-Fi Pi** | **존재함.** Pi 포크, provider-aware 에이전트 런타임 | `~/Gunnsplayground/Hi-Fi-Pi` (TS 모노레포) |
| **Rhizome** | **제안일 뿐. 코드 없음.** 능동 stateful 브레인 | — |
| **JARVIS** | **제안일 뿐. 코드 없음.** 얇은 통합 UI | — |

다음 세션은 Rhizome·JARVIS가 **아직 없다**는 걸 전제로 논의한다.

---

## 1. 네 프로젝트의 역할

**Hi-Fi Pi (런타임)** — 실제로 LLM과 대화하고 툴을 부르는 것. 멀티벤더
(OpenAI·Gemini·Bedrock·xAI·Anthropic), provider-native 입력, 무침묵-변환,
모델 전환 이식성. 패키지: `agent · ai · client · coding-agent · evals ·
protocol · server · session-backends · storage · telemetry · tui`. 단독으로도
완결(코딩 에이전트 CLI/TUI).

**HX / JANUS (거버넌스 기판)** — 한 에이전트가 무엇을 했나를 격리·기록·승인.
rootless OCI 샌드박스, append-only 이벤트 로그(단일 writer·seq 전순서),
정책(교집합 병합), 승인 handshake(기록 후 응답), 효과 평면 수집(collector),
OTel export. CLI: `hx run/replay/fork/audit/dump-config`. 어댑터로 외부
에이전트를 감싼다(claude·codex 존재; hifi-pi는 미구현 확장점). 단독으로도
완결(거버넌스 도구).

**Rhizome (능동 브레인)** — 기억·목표·미션의 stateful 상태기계. **능동**:
월요일 트리거에 스스로 깨어나 미션을 만들고 끝까지 민다. 오케스트레이션 제어
(다음 단계·어느 에이전트·언제 사람에게·목표 달성 판정)를 소유. event-sourced.
HX를 구동하고 HX 로그를 관측해 미션 상태를 갱신.

**JARVIS (얇은 얼굴)** — 관제실 UI. Rhizome을 렌더하고 사람 입력(승인 등)을
relay. 로직 없음 — 로그·상태의 사영일 뿐. 갈아끼울 수 있고(web/TUI/mobile),
JARVIS가 꺼져도 미션은 Rhizome에서 계속 진행.

---

## 2. 결정된 구조 원칙

**멀티레포 — 접점이 이미 언어를 가로지른다.** Go 기판 vs TS 런타임 vs (미정)
브레인/UI. 합치면 빠른 것(프로바이더·UI)이 느린 것(기판 불변식)을 흔든다.

**결합 = 함수 import가 아니라 인터페이스로 능력을 씀.**
- 공유 계약(스키마·프로토콜) = 유일한 진짜 import. 스키마-우선, 각 언어가
  codegen (HX의 "스키마가 진실, 타입은 codegen"이 이걸 예비함).
- 런타임 능력 = JARVIS→Rhizome, Rhizome→HX는 CLI/이벤트(NDJSON);
  HX→Hi-Fi Pi는 어댑터(서브프로세스).
- 내부 코드에 손 뻗기 = 절대 금지. 리트머스: **"HX 내부를 바꿔도 Rhizome을
  안 건드릴 수 있나?" 그렇다 → 진짜 멀티레포. 아니다 → 분산 모놀리스.**

**의존 방향 (사이클 없음):**
```
                contracts (공유)
              /       |        \
   Hi-Fi Pi        HX/JANUS     Rhizome
        ▲ 어댑터        ▲ CLI/이벤트  │ 구동·관측
        └──── HX ───────┘◄───────────┘
                                       │
                                  JARVIS (UI)
```
Hi-Fi Pi와 Rhizome은 직접 안 만남(HX가 사이). JARVIS는 Rhizome 하나만
백엔드로 봄(단일 통합점).

**거버넌스 규칙 — 통치 경로는 반드시 HX를 지난다.** JARVIS/Rhizome이
Hi-Fi Pi를 직접 부르면 격리·감사·승인 우회(claude 직접 실행과 같음).
통치 경로: Rhizome → HX → 어댑터 → Hi-Fi Pi. 개발 경로: Hi-Fi Pi 단독(비통치).

**Rhizome은 능동 브레인이다(저장소 아님).** 결정: 스스로 미션을 진행시킨다.
따라오는 의무 셋:
1. **event-sourced** — 미션 결정도 로그에서 파생, "왜 이 계획?" 답 가능.
2. **권위의 뿌리가 아니라 첫 좁힘 단계** — 사람/조직 천장 안에서만 배분.
3. **긴 수명 서비스** — 재시작 시 이벤트 로그에서 미션 상태 재생, 자기 루프
   실패 처리.

**Rhizome의 뇌 ≠ 에이전트의 뇌.** 오케스트레이션 제어는 Rhizome, 도메인 추론
(무엇을 쓸지)은 에이전트(HX→Hi-Fi Pi). 두 종류의 "생각"을 섞지 마라.

---

## 3. 끊기지 않아야 할 두 사슬

이게 4-프로젝트 구조가 지키는 것이다.

**좁힘 사슬** (권한은 좁아지기만):
```
사람/조직 정책(천장) → Rhizome 미션 → HX 세션 → 서브에이전트
```
어디서도 안 넓어짐. Rhizome이 천장보다 넓게 못 준다.

**감사 사슬** (무슨 일이 있었나 되짚기):
```
사람 목표 → Rhizome 미션 결정(로그) → HX 세션(로그) → 에이전트 행동(로그)
```
에이전트 툴 콜 → HX 승인 → Rhizome 미션 → 사람 목표까지 끝까지 추적.

---

## 4. 두 상태의 경계 (표류 방지)

HX도 "로그", Rhizome도 "상태"를 가진다. 도메인이 달라야 충돌 없다:

| | 무엇의 진실 |
|---|---|
| **HX 로그** | "실행에서 **무슨 일이 일어났나**" — 불변, append-only, 세션당 |
| **Rhizome** | "**무엇을 하려 하고 무엇을 기억하나**" — 목표·기억·미션 의도, 가변, 세션초월 |

Rhizome은 실행 사실을 재기록하지 않는다 — HX 세션을 ID로 참조하고 그 위에
목표·기억을 얹는다.

---

## 5. 명세 논의로 열린 질문 (다음 세션)

우선순위 순:

1. **계약 화해 — 멀티레포 성립의 첫 실측.** HX `contracts/`(§5.2 이벤트·와이어
   스키마)와 Hi-Fi Pi `packages/telemetry`(span 기반, OTel-adaptable) +
   `packages/protocol`을 실제로 대조. 하나의 공유 계약으로 정렬 가능한가,
   아니면 매핑 층이 필요한가. **둘 다 OTel 계열**이라 span/이벤트 의미론에서
   만날 가능성. 이게 안 되면 멀티레포 전체가 재검토 대상.
2. **공유 계약이 어디 사나.** 새 레포로 추출? JANUS `contracts/`에서 분리?
   4-way 조율이므로 계약은 느리게 변하는 것만(이벤트·와이어·정책 타입).
3. **hifi-pi 어댑터** (HX의 FR-ADP-09 확장점) — HX가 Hi-Fi Pi를 서브에이전트로
   감싸는 어댑터. 미구현. claude·codex 어댑터가 선례.
4. **Rhizome 첫 계약** — HX 로그를 어떻게 관측하고(read), 미션을 어떻게
   HX에 구동하는지(hx run/fork). 능동 루프의 실제 API.
5. **HX 자체 잔여** — `hx run`의 샌드박스 CLI 배선(현재 호스트 경로) +
   대화형 decider(현재 DenyAll 하드코딩). Rhizome이 사람 승인을 relay하려면
   이 decider 자리가 열려야 한다.
6. **Rhizome 능동 서비스 설계** — event-sourced 재생, 동시 미션, 트리거/
   스케줄러, 루프 실패·재시도.
7. **JARVIS 표면** — 얇은 UI. Rhizome 단일 백엔드, 승인 relay.

---

## 6. HX v0.1 현재 상태 (기판이 이미 주는 것)

- §8 릴리스 게이트: 여섯 완전 + 2번(어댑터) Claude·Codex 실 세션 실증.
- 문서화된 잔여: Claude 컨테이너 실 토큰(리눅스 호스트 몫), FR-SBX-04 스코프
  (벤더 공통 한계), codex `--sandbox` 컨테이너 안 작동(리눅스 CI 후속),
  `hx run` 샌드박스·decider CLI 배선.
- 근거: `docs/v0.1-release-acceptance.md`, `docs/traceability.md`,
  `docs/hx-기능명세서-v0.1.md`(원본 명세), `docs/as-built`(아티팩트).

---

## 한 문장

**Hi-Fi Pi(런타임)를 HX(거버넌스)가 감싸고, Rhizome(능동 브레인)이 사람 천장
아래에서 미션으로 HX를 구동하며, JARVIS(얇은 UI)가 그걸 사람에게 보여준다 —
넷이 하나의 공유 계약을 축으로 독립·완결하며, 좁힘 사슬과 감사 사슬이 사람부터
에이전트까지 끊기지 않는다.**
