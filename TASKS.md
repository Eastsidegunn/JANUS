# TASKS.md — HX 구현 작업 큐

규칙: 위에서 아래로, 한 번에 하나. 각 태스크의 "완료 기준"이 CI green이 되어야 다음으로 넘어간다.
[H] 표시는 사람이 직접 하거나 사람 리뷰가 필수인 태스크.

---

## T0. 저장소 강제 장치 [H: 설정 리뷰]
- 내용: 모노레포 초기화(contracts/core/seams/collector/surfaces), 경계 린트 설정(의존 방향 규칙 §3.1, seam 수평 import 금지), CI 파이프라인(lint→test→fixtures), Makefile.
- 대상: NFR 전반, 불변식 5
- 완료 기준: 고의로 위반하는 더미 import를 넣으면 CI가 실패하고, 제거하면 통과한다.

## T1. contracts 스키마 + codegen [H: 스키마 최종 리뷰 필수]
- 내용: `events.schema.json`(§5.1), `wire.schema.json`(§5.2) 작성. JSON Schema → Go 타입 codegen 파이프라인. kind 어휘 전체 포함.
- 대상: FR-LOG 스키마, FR-ADP-01, §5 전체
- 완료 기준: codegen 산출 타입으로 §5.1 예시 이벤트들이 파싱·검증된다. 스키마 위반 샘플은 거부된다.
- 주의: 이 저장소에서 가장 되돌리기 비싼 산출물. 사람 리뷰 없이 머지 금지.

## T2. 속성 테스트 골격 (실패 상태로 커밋)
- 내용: FR-LOG-06(리플레이 결정론), FR-POL-03(병합은 좁아지기만)의 속성 테스트를 구현 없이 먼저 작성. CI에서 expected-fail로 표시.
- 완료 기준: 두 테스트가 존재하고, 임의 입력 생성기가 이벤트 시퀀스/프로파일 조합을 실제로 생성한다.

## T3. logd — 단일 writer + append-only 저장소
- 내용: SQLite WAL, events 테이블(§5.1), UPDATE/DELETE 차단 트리거, writer의 seq 발급, fsync 내구성, 백프레셔 인터페이스.
- 대상: FR-LOG-01/02/07/08/09, NFR-02/03
- 완료 기준: append-only 트리거 테스트, 동시 쓰기 시도 시 writer 경유 강제 테스트, redaction 기본 패턴 테스트 green.

## T4. 프로젝션 + 리플레이 + 포크
- 내용: deriveMessages 등 프로젝션, `replay(events)` 재계산, seq 지점 포크(새 trace_id, 원본 참조).
- 대상: FR-LOG-03/04/05/06
- 완료 기준: T2의 리플레이 결정론 속성 테스트가 expected-fail에서 green으로 전환. 포크 후 원본 불변 테스트 green.

## T5. loop — turn/step 상태 머신 + 훅 4지점
- 내용: 고정 상태 머신, pre_step/pre_tool/post_tool/turn_stopping, 판정 모델(continue|rewrite|reject)과 충돌 해소(reject>rewrite>continue), 판정의 이벤트 기록.
- 대상: FR-LOOP 전체
- 완료 기준: 훅 판정 조합 테이블 테스트, reject된 첫 step이 step 없는 durable turn으로 남는 테스트 green.

## T6. policy — 프로파일 파서 + 순수 평가 함수
- 내용: YAML 프로파일(§FR-POL-01 필드), (프로파일, spawn 요청)→거부|샌드박스 설정, 교집합/최솟값 병합.
- 대상: FR-POL-01/02/03/06
- 완료 기준: T2의 병합 협소성 속성 테스트 green. 예산 초과 판정 단위 테스트 green.

## T7. null 어댑터 + 워킹 스켈레톤
- 내용: 와이어 프로토콜(§5.2)을 말하는 스크립트 기반 가짜 어댑터. `hx run`→spawn→NDJSON 파싱→writer→child span→`hx replay` 관통.
- 대상: FR-ADP-01/02/03, FR-CLI-01/02, FR-LOG-10
- 완료 기준: null 어댑터 세션의 E2E 테스트 — run 후 replay가 동일 상태, 자식 중간 이벤트가 부모 모델 히스토리에 미포함.

## T8. 픽스처 녹화 [H: 사람이 실행]
- 내용: Claude Code, Codex의 실제 출력 녹화 15~20 시나리오(정상/툴 다수/승인 요청/에러/중단). `contracts/fixtures/`에 커밋.
- 대상: FR-ADP-05의 전제
- 주의: 실 자격증명 필요. 에이전트에게 위임하지 않는다.

## T9. Claude Code 어댑터
- 내용: stream-json → 정규화 이벤트 변환, raw passthrough, usage 보고, 관측 가능 등급 선언, 승인 요청 승격.
- 대상: FR-ADP-03/04/05/06/07, FR-POL-05
- 완료 기준: T8 픽스처 전체에 대한 스냅샷 테스트 green.

## T10. world local 백엔드 — 컨테이너 실행
- 내용: OCI 컨테이너(rootless) spawn, 워크스페이스 overlayfs 마운트, egress 기본 차단 + 프록시, 단기 자격증명 주입, 어댑터는 호스트 측. 실행 환경 메타데이터의 spawn 이벤트 기록.
- 대상: FR-SBX 전체, FR-ADP-10
- 완료 기준: allowlist 밖 도메인 접근 차단 통합 테스트(§8-4), 컨테이너 내부에서 어댑터 프로세스 접근 불가 확인.

## T11. collector — fsdiff + egress
- 내용: overlayfs upper 기반 변경 파일 목록 → `collector/fs_changed`, 프록시 로그 → `collector/egress`(메타데이터만). span 귀속, writer 경유.
- 대상: FR-COL-01/02/03/05/06
- 완료 기준: 에이전트가 보고하지 않은 파일 변경이 collector 이벤트로 잡히는 통합 테스트 green.

## T12. hx audit — 대조 리포트
- 내용: 의도 평면 vs 효과 평면 대조, 불일치 3분류(보고-무관측/관측-무보고/일치), span·비용 질의.
- 대상: FR-AUD 전체, FR-CLI-04
- 완료 기준: 인위적 불일치 시나리오(null 어댑터가 거짓 보고) 검출 테스트 green — §8-3.

## T13. 확장 패스스루 — 프로비저닝 단계
- 내용: extensions 선언 파싱(해시 고정 검증), 프로비저닝/실행 프로파일 분리, 설치 세트의 spawn 이벤트 기록, 콘텐츠 주소 캐시.
- 대상: FR-EXT 전체
- 완료 기준: §8-8 — 프로비저닝 중 레지스트리 접근 성공, 실행 중 동일 도메인 차단, 확장 세트 기록 확인.

## T14. OTel export + Codex 어댑터 + dump-config
- 내용: trace/span export(FR-OBS-01/02), Codex 어댑터(T8 픽스처 기반), `hx dump-config`.
- 완료 기준: Jaeger 렌더링 확인(§8-7), Codex 픽스처 테스트 green.

---

## 이후 (T15+, 착수 전 사람 판단 필요)
- pi / OpenClaw / 사내 에이전트 어댑터 (contracts 안정성의 성적표)
- exec 감사(eBPF), Postgres store, 규칙 엔진 — 전부 명세 부록 A의 미결 확정 후.
