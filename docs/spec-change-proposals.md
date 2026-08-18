# 명세 변경 제안 기록

명세(docs/hx-기능명세서-v0.1.md)는 유일한 진실이므로 구현자가 직접 수정하지 않는다.
변경이 필요하다고 판단되면 여기에 제안만 기록하고, 명세 소유자의 승인 후 반영한다.

---

## SCP-001. §3.1 도식의 화살표 방향 표기 (상태: 제안됨, 2026-08-16)

- 위치: §3.1 정적 구조 도식 `contracts → core → seams → surfaces`
- 문제: §7 불변식 6은 `contracts ← core ← seams ← surfaces`(의존 방향)로 표기해
  같은 구조가 문서 내에서 반대 화살표로 두 번 등장한다. 도식을 의존 방향으로
  읽으면 오해를 부른다.
- 제안(택1):
  1. 도식의 화살표를 `←`로 뒤집어 의존 방향으로 통일.
  2. 도식은 유지하되 "화살표는 층의 순서이며 의존 방향은 그 역"이라는 각주 추가.
- 구현 영향: 없음 — boundarylint는 이미 의존 방향(불변식 6) 기준으로 강제 중.

---

## SCP-002. §5.2 코어→어댑터 command에 `approval_response` 추가 (상태: **명세 변경 요청**, 2026-08-18 갱신)

- 위치: §5.2 "코어 → 어댑터 (stdin)" 표(현재 `task`/`message`/`stop`),
  FR-ADP-02("spawn / send / events / stop의 최소 계약").
- 문제: FR-POL-05는 승인 요청이 부모 정책 레이어의 **판정**으로 이어질 것을
  요구하지만, 판정을 어댑터로 되돌릴 command가 없어 판정이 실행을 게이트할
  경로가 없다.
- **제안(단일안 — 2차 리뷰 지시로 해석 승인안 철회)**: 명세를 변경한다.
  1. §5.2 command 표에 `approval_response` 행 추가
     (payload: request_id, decision(allow|deny), reason — deny 시 필수)
  2. FR-ADP-02의 최소 공통 계약을 spawn / send / events / stop / **approval**로
     확장 명시
  3. 위 명세 변경 승인 후에만 `wire.schema.json`·codegen 반영
- 근거: 이 저장소는 기능 명세를 유일한 진실로 선언한다. §5.2 표를 그대로 둔 채
  해석 기록만으로 command를 추가하면 contracts와 명세가 다시 갈라진다.
