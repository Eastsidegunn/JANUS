# T5 payload 스키마 확정 제안 — 대화·툴 kind와 지점별 rewrite 대상

상태: **승인 대기** (2026-08-17). contracts/는 [H] 영역이므로 이 문서 승인 전에는
수정하지 않는다. 승인 시 events.schema.json에 additive로 반영하고(§5.1 확장
규칙 준수 — 기존 유효 인스턴스를 깨는 변경 없음… 단 아래 §3의 폐쇄화는 예외로
명시 승인 필요), schemagen 재생성·validate 회귀·loop 방출 조정을 같은 커밋으로
진행한다.

## 1. 배경

T1 스키마는 payload 형태가 명세에 없는 18종 kind를 열린 객체로 두고 "해당
태스크에서 additive로 확정"을 `$comment`로 남겼다. T5가 loop를 구현하면서
대화·툴 kind의 payload 형태가 core 타입(hand-written)으로 생겼고, 리뷰 지적대로
이대로 두면 스키마와 코어 타입이 별도의 진실이 된다.

## 2. 확정 제안 — payload $defs (additive)

```
userMessagePayload:      { text: string }                              required [text],  additionalProperties false
assistantMessagePayload: { text: string }                              required [text],  additionalProperties false
toolCallPayload:         { name: string(minLength 1), args: object }   required [name, args], additionalProperties false
toolResultPayload:       oneOf (판별 필드 status):
  ├ { status: "ok",       output: object }                             required [status, output]
  ├ { status: "rejected", reason: string(minLength 1) }                required [status, reason]  ← 훅 거부 (FR-LOOP)
  └ { status: "error",    error: string(minLength 1) }                 required [status, error]   ← 툴 실행 오류
turnBoundaryPayload:     { }  (빈 객체, additionalProperties false)    ← turn/start, turn/end, step/start, step/end 공용
```

- 전부 schemagen 승인 서브셋 안이고, toolResultPayload의 oneOf는 단일 판별
  const(status) 규칙을 지킨다.
- **loop 방출 조정 필요**: 현재 tool/result는 `{"output":…}` / `{"rejected":true,…}` /
  `{"error":…}` 3형태(비판별) → status 판별로 변경. tool/call의 `args`는 현재
  모델이 args를 안 주면 `null`이 될 수 있음 → loop가 `{}`로 정규화.
- **output을 object로 제한**: 툴이 스칼라를 반환하면 어댑터/툴 계층이
  `{"value": …}`로 감싼다. (대안: output을 string(직렬화 JSON)으로 — 스키마
  표현력은 낮지만 제약 없음. 기본 제안은 object, 리뷰에서 택일 요청.)

## 3. 폐쇄화의 성격 (승인 필요 지점)

위 확정은 "열린 객체 → 폐쇄 객체"이므로 엄밀히는 좁히는 변경이다(기존에
유효하던 임의 payload가 무효가 됨). §5.1 확장 규칙("새 모델 가시 입력은 새
kind")과는 충돌하지 않으나 — 필드 추가가 아니라 형태 확정이므로 — T1 때
`$comment`로 예고된 확정 절차의 이행이다. 이 성격을 명시 승인 대상으로 둔다.

## 4. 지점별 rewrite 대상 (hookVerdictPayload.rewrite)

| 지점 | rewrite 대상 | 스키마 강제 |
|---|---|---|
| pre_step | 모델 요청 `{messages: […]}` — 형태는 core/loop 소유(프로젝션 타입) | 루프 strict decode만 |
| pre_tool | toolCallPayload | 루프 strict decode (스키마 참조 가능해짐) |
| post_tool | toolResultPayload의 `status:"ok"` 분기 | 루프 strict decode |
| turn_stopping | userMessagePayload (주입 입력) | 루프 strict decode |

스키마 수준에서 지점별 rewrite 형태를 강제하려면 point×verdict 이중 판별이
필요해 schemagen 계약(분기당 판별 const 정확히 1개) 밖이다. 제안: rewrite는
`{"type":"object"}`를 유지하고 위 표를 `$comment`로 스키마에 기록, 실행 강제는
루프의 strict decode(zero-value 교체 + DisallowUnknownFields — T5 재리뷰 반영
완료)가 담당한다. pre_step 대상(모델 요청)은 코어 소유 형태라 contracts에
넣지 않는다 — 넣으면 contracts가 루프 내부 표현에 역결합된다.

## 5. 승인 시 작업 목록 (동일 커밋)

1. events.schema.json: §2의 $defs 추가, 해당 kind 분기의 payload `$ref` 연결,
   rewrite `$comment` 갱신 (18종 중 8종 확정: user/assistant message,
   tool/call·result, turn/step 경계 4종)
2. `make codegen` 재생성 (drift 게이트 통과)
3. loop 방출 조정: tool/result status 판별화, args `{}` 정규화
4. validate 회귀: 각 payload의 유효/위반 샘플 (부분·빈·미지 필드)
5. T2 이벤트 생성기의 해당 kind payload를 확정 형태로 갱신
