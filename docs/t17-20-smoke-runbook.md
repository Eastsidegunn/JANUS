# T17~20 실 토큰 smoke 런북 — 접수→관측→승인→중단 관통 ([H] 전용)

목적: 접수(T17)→관측→승인 relay(T18)→중단(T19)→proxy 주입(T20) 전체 사슬을 실 토큰 단일 세션으로 1회 관통.

> **[H] 전용.** 되돌릴 수 없는 실 토큰·외부 효과이므로 사람이 직접 수행한다(결정 10번). 에이전트에 위임하지 않는다. 실 토큰은 world-config JSON 파일 안에만 두고 CI·원격에 보내지 않는다.
>
> 이 문서는 실제 CLI 플래그(`cmd/smoke/main.go`, `surfaces/hx/main.go`·`stop.go`, `cmd/rhizome/main.go`)와 대조해 재구성했다(2026-09-15). 어느 단계든 실패하면 그대로 멈추고 출력 전문을 리뷰어에게. 모든 단계 재실행 안전.

## 0. 원리

- 드라이버는 journal 잠금을 잡으므로 `rhizome serve`가 떠 있으면 smoke 명령이 거부된다 — 규율이 실수를 막는다.
- 루프는 `smoke tick` 1회씩. 각 단계에서 무엇이 기록되는지 `hx replay`로 눈으로 따라간다.
- 승인은 `smoke approve` — 사람이 직접 실행하는 결정이고 actor로 남는다(Gunnflow 버튼은 RHZ-047 이후).
- JANUS의 판단 근거는 소켓 응답이 아니라 **세션 로그 관측**이다(제2원칙).

## 1. JANUS 준비 (hx 빌드 — `make build` 타깃 없음, go build 직접)

```sh
cd ~/Gunnsplayground/JANUS && git pull
go build -o /tmp/hx ./surfaces/hx
HX=/tmp/hx
PROFILE=<정책 YAML 절대경로>        # T16 때 쓴 프로파일
WORLD=<world-config JSON 절대경로>  # T20: 토큰 없음. env(평문)+inject 규칙(credential 이름만)만
SMOKE=~/rhizome-smoke && mkdir -p "$SMOKE/accept"
SOCK=$SMOKE/approval.sock            # 소켓 부모 디렉터리는 0700이어야 함(peer fail-closed)
chmod 700 "$SMOKE"
```

**profile-hash 계산** (중요 — dump-config 출력이 아니라 파일 내용 pin 해시다. hx가 재검증하는 값이며, 아래 식이 hx의 `pinnedProfileHash`와 바이트 일치함을 검증했다):

```sh
PHASH=$(python3 -c '
import hashlib,sys
def uv(n):
 b=b""
 while True:
  x=n&0x7f; n>>=7
  b+=bytes([x|0x80]) if n else bytes([x])
  if not n: break
 return b
h=hashlib.sha256()
for p in sys.argv[1:]:
 d=open(p,"rb").read(); h.update(uv(len(d))); h.update(d)
print(h.hexdigest())
' "$PROFILE")   # overlay를 쓰면 profile 다음에 순서대로 인자 추가
echo "$PHASH"
```
> profile/overlay 파일이 이 시점 이후 바뀌면 hx가 `POLICY_CHANGED`로 거부한다 — pin은 실행 순간의 파일 내용에 고정된다.

### 1-T20. proxy 보유 자격증명 (T20/T22 — 실 토큰은 world-config에 없다, 컨테이너엔 placeholder만)

T20 이후 **실 토큰은 world-config JSON에 넣지 않는다.** hx run이 `/dev/tty`(ECHO off)로 값을 직접 받아 proxy에만 전달하고, 컨테이너/argv/log 어디에도 **실 토큰 값**이 없다.

**T22 보정(중요):** 컨테이너의 claude-code는 `CLAUDE_CODE_OAUTH_TOKEN` env가 **없으면** "Not logged in"으로 API 요청 자체를 만들지 않는다([H] 실측: dummy 값을 주면 401 = env를 읽어 `Authorization: Bearer …` 요청을 실제로 생성). 그래서 컨테이너 env에 **비밀이 아닌 placeholder** `CLAUDE_CODE_OAUTH_TOKEN`을 준다 → claude가 로그인 판정을 통과해 `Authorization: Bearer <placeholder>` 요청을 만든다 → **proxy가 그 Authorization을 실 토큰으로 replace**(`proxy.go`의 `Header.Set`이 이미 replace이므로 proxy 코드 변경 없음). 결과: **컨테이너엔 placeholder 값만, 실 토큰 값은 proxy만 보유**하는 무비밀 원칙 유지. placeholder는 sentinel(실 토큰)과 명백히 다른 고정 비밀-아닌 문자열이어야 한다.

world-config의 어댑터 항목은 이름·설정·placeholder만:
```json
"claudecode": {
  "bin": "...", "image": {...}, "agent_argv": ["claude"], "control_mode": "tool_approval",
  "env": [
    "HTTP_PROXY=http://hx-egress-proxy:3128",
    "ANTHROPIC_BASE_URL=http://api.anthropic.com",
    "CLAUDE_CODE_OAUTH_TOKEN=hx-proxy-injected-placeholder"
  ],
  "inject": [{ "domain": "api.anthropic.com", "header": "Authorization", "value_prefix": "Bearer ", "credential": "CLAUDE_CODE_OAUTH_TOKEN" }]
}
```
- `env`: 평문·비밀 아님만. `HTTP_PROXY`는 **정적 alias `hx-egress-proxy`**(:3128), `ANTHROPIC_BASE_URL`은 **평문 http + 실 도메인**(주입 대상은 CONNECT deny라 평문 forward로 proxy에 닿아야 함), `CLAUDE_CODE_OAUTH_TOKEN`은 **placeholder 값**(로그인 판정 통과용, 실 토큰 아님).
- `inject[].credential`은 **이름만**. 값은 hx run 실행 중 `/dev/tty` 프롬프트로 입력해 proxy가 placeholder를 이 실 토큰으로 replace한다. env의 placeholder와 inject의 credential이 같은 이름(`CLAUDE_CODE_OAUTH_TOKEN`)이어도 무방하다 — env는 placeholder 값(컨테이너), inject는 실 값(proxy 전용)으로 **출처·저장소가 완전히 독립**이다(`worldAdapterConfig`의 `env`/`inject`는 별개 필드).
- **열린 항목(리뷰어/드라이버 확인 필요)**: hx run은 tty 프롬프트를 띄우는데, smoke 드라이버(`cmd/smoke`→janusadapter가 hx run을 서브프로세스로 실행)가 그 tty 프롬프트를 [H] 터미널로 통과시키는지 미확인. 통과 안 되면 (a) 드라이버가 tty를 상속·전달하도록 하거나, (b) hx run을 smoke start와 분리해 [H]가 직접 실행. 서버 검증 전 이 경로 확정 필요.

## 2. Rhizome 준비

```sh
cd ~/Gunnsplayground/Rhizome
go build -o /tmp/smoke ./cmd/smoke
J=$SMOKE/journal.ndjson
/tmp/smoke mission -journal "$J" -name mission-smoke1
KEY=smoke-$(date +%s)        # idempotency key — 정본 세션 경로 계산에 재사용하므로 변수로 고정
/tmp/smoke exec -journal "$J" -mission mission-smoke1 -key "$KEY"
# 출력된 execution ID 기록:
EXEC=exec-smoke-...
```

**정본 세션 경로 계산** (중요 — hx run의 `--session`은 `<accept-root>/<keyhash>/session.sqlite` **정본 경로만** 받는다. 임의 경로를 주면 `KEY_CONFLICT`. keyhash=`sha256(uvarint(len(scope))‖scope‖uvarint(len(key))‖key)`, 아래 식이 hx의 `accept.KeyHash`와 바이트 일치함을 검증했다. scope는 아래 start의 `-scope` 값과 동일해야 한다):

```sh
SCOPE=smoke
KEYHASH=$(python3 -c '
import hashlib,sys
def uv(n):
 b=b""
 while True:
  x=n&0x7f; n>>=7
  b+=bytes([x|0x80]) if n else bytes([x])
  if not n: break
 return b
h=hashlib.sha256()
for s in sys.argv[1:]:
 h.update(uv(len(s))); h.update(s.encode())
print(h.hexdigest())' "$SCOPE" "$KEY")
SESSION="$SMOKE/accept/$KEYHASH/session.sqlite"
echo "$SESSION"
```
> 대안: `-session-db`를 아예 생략하면 hx가 이 정본 경로를 계산·생성하고 accepted NDJSON의 `session_ref.session_db`로 반환한다. 단 현재 smoke 드라이버(`internal/janusadapter/hx.go`)는 `-session-db`를 `--session`에 그대로 넘기고 accepted 응답을 조회하지 않으므로, 드라이버를 안 고칠 거면 위처럼 **정본 경로를 명시**해야 한다.

## 3. 시작 (접수 — T17)

```sh
/tmp/smoke start -journal "$J" -exec "$EXEC" -hx "$HX" \
  -profile "$PROFILE" -accept-root "$SMOKE/accept" -world-config "$WORLD" \
  -session-db "$SESSION" -approval-endpoint "$SOCK" \
  -operation-id op-smoke1 -scope "$SCOPE" -fingerprint fp-smoke1 \
  -adapter claudecode -workspace-ref /workspace \
  -instruction "작업 디렉터리 파일 목록을 요약하라" \
  -profile-id <PROFILE의 id 필드값> -profile-hash "$PHASH"
```
기대: `status=accepted ... external=<trace_id>`. **같은 명령을 다시 치면 `duplicate=true`** — 무중복 spawn 증명(경로가 이미 key를 배타 점유).

## 4. 관측

```sh
/tmp/smoke tick -journal "$J" -hx "$HX" -approval-endpoint "$SOCK"
/tmp/smoke status -journal "$J"
```
세션이 승인을 요청하면 `$HX replay --session "$SESSION"`에서 `subagent/approval_request`의 (trace_id, span_id, request_id) 세 값과 요청 digest를 확인해둔다(5단계 입력).

## 5. 승인 (사람의 결정 — T18)

```sh
/tmp/smoke approve -journal "$J" -trace <trace> -span <span> -request <request_id> \
  -response-id resp-smoke1 -digest <hx-args-digest-v1:...>
/tmp/smoke tick -journal "$J" -hx "$HX" -approval-endpoint "$SOCK"   # 제출
/tmp/smoke tick -journal "$J" -hx "$HX" -approval-endpoint "$SOCK"   # durable 관측
/tmp/smoke status -journal "$J"
```
기대: gate에 `human=` actor가 붙으면 관통 성립. JANUS 쪽 근거는 소켓 응답이 아니라 세션 로그의 durable `policy/decision`(actor_ref, decision_source=relay) 관측이다(제2원칙). digest는 4단계 replay에서 본 요청의 `hx-args-digest-v1:` 값 그대로.

## 6. 중단 (T19)

```sh
/tmp/smoke stop -journal "$J" -exec "$EXEC" -reason user
/tmp/smoke tick -journal "$J" -hx "$HX" -approval-endpoint "$SOCK"   # 소켓 제출(stop_accepted)
/tmp/smoke tick -journal "$J" -hx "$HX" -approval-endpoint "$SOCK"   # done stopped 관측
/tmp/smoke status -journal "$J"
```
기대: `cancelled` 계열. **tick 한 번 더 필요한 게 정상** — 종결 사실(`subagent/done status=stopped`)은 접수 응답이 아니라 관측에서만 확정된다. (reason `budget_exceeded`/`policy`는 로그 입증 조건이 있어야 수용, `policy`는 `-evidence-seq` 필수.)

## 7. 관제 최종 확인

```sh
cd ~/Gunnsplayground/Rhizome && go build -o /tmp/rhizome ./cmd/rhizome
/tmp/rhizome serve -journal "$J" -addr 127.0.0.1:8790
# 다른 터미널:
curl -s localhost:8790/v1/execution/mission-smoke1 | python3 -m json.tool
# Gunnflow real 모드 접속 시 mission·gate·세션 상태 화면 확인
```
> serve는 journal 잠금을 잡으므로 실행 중엔 smoke 명령을 쓸 수 없다(0절 원리).

## 8. 성공 판정 5기준

1. `accepted` + 재실행 `duplicate=true` (무중복 spawn — T17)
2. gate에 human actor / JANUS durable `policy/decision`이 relay·사람 표식과 함께 기록 (T18)
3. `cancelled`이 replay 관측 경유로만 확정 (T19, 제2원칙)
4. 자동 allow·자동 승인 경로가 어디에도 없음 (승인은 사람의 approve만)
5. 관제 표면(`/v1/execution`)이 각 단계를 사실대로 표시

## 알려진 함정 / 후속

- **profile-hash**: 반드시 1단계의 pin 계산값. dump-config 출력(=policy_hash)을 쓰면 `POLICY_CHANGED`. hx가 이 pin을 직접 내보내지 않는 것은 후속 개선 후보(`hx dump-config --pin` 등).
- **operation_id/correlation_id**: stop-request에서 파싱만 되고 wire 미전송(v2 항목) — smoke 흐름엔 영향 없음.
- **DenyAll 세션**: `-approval-endpoint`를 생략하면 소켓이 없어 5·6단계 불가. 관통 smoke는 반드시 endpoint 지정.
- 실패 시 accept 레지스트리(`$SMOKE/accept`)와 `session.db`는 남겨두고 전문과 함께 보고 — tombstone·claim 상태는 `$HX audit-accept --accept-root "$SMOKE/accept" --scope "$SCOPE" --key "$KEY"`로 관측.
