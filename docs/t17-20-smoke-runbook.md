# T17~19 + T23 외부 gateway smoke 런북 — 접수→관측→승인→중단 관통 ([H] 전용)

목적: 접수(T17)→관측→승인 relay(T18)→중단(T19)과 CLIProxyAPI 표준 API 호출(T23)을 단일 세션으로 관통. T20/T22 주입·placeholder 절차는 T23으로 대체·외부화됐다.

> **[H] 전용.** 되돌릴 수 없는 실 토큰·외부 효과이므로 사람이 직접 수행한다(결정 10번). 에이전트에 위임하지 않는다. 구독 OAuth 토큰은 CLIProxyAPI에만 두고 JANUS에 전달하지 않는다. world-config에는 CLIProxyAPI 접근키만 평문으로 두며 저장소·CI에 올리지 않는다.
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
WORLD=<world-config JSON 절대경로>  # T23: gateway URL + 접근키 env(평문), inject 없음
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

### 1-T23. 외부 CLIProxyAPI + 접근키 env

CLIProxyAPI 배치·OAuth 로그인·refresh·벤더 봉투는 운영자 책임이다. JANUS는 실행하거나 인증하지 않으며 표준 Anthropic API endpoint만 사용한다. 아래는 기존 world-config의 어댑터 항목에 넣는 **env 부분**이다. 예시 키는 실제 키가 아니다.

```json
"env": [
  "ANTHROPIC_BASE_URL=http://cliproxy.operator.example:8317",
  "ANTHROPIC_AUTH_TOKEN=REPLACE_WITH_CLIPROXY_ACCESS_KEY"
]
```

`ANTHROPIC_AUTH_TOKEN`을 선택한 근거: [Claude Code 공식 gateway 문서](https://code.claude.com/docs/en/llm-gateway-connect)는 이 변수를 `Authorization: Bearer`로, `ANTHROPIC_API_KEY`를 `x-api-key`로 전달한다고 명시한다(2026-09-24 확인). JANUS proxy는 클라이언트 헤더를 그대로 전달한다. gateway가 x-api-key를 요구하면 API_KEY로 바꿀 수 있지만 서로 다른 키 두 개를 동시에 선언하지 않는다. 중복 env 이름·NUL·`CLAUDE_CODE_OAUTH_TOKEN`은 설정 오류다. `inject` 필드는 제거되어 기존 설정은 명시적으로 거부된다. tty 입력·placeholder·replace는 없다.

profile의 `egress`에는 CLIProxyAPI의 전용 DNS 이름만 선언한다:

```yaml
egress:
  - cliproxy.operator.example
```

기존 proxy 설정은 유지된다. agent는 per-span internal network에만 연결되고 HTTP_PROXY/HTTPS_PROXY는 JANUS sidecar를 가리킨다. BASE_URL은 **CLIProxyAPI** 주소이며 sidecar 주소가 아니다. proxy는 DNS 결과를 고정해 dial하며 private/loopback/link-local/metadata/IP literal을 계속 거부한다. 따라서 `localhost`, RFC1918 서버 주소로는 이 경로가 성립하지 않는다. 운영자가 현재 규칙에 맞는 공인 IP로 해석되는 endpoint를 제공해야 한다. HTTP forward는 지정 포트를 유지하며 TLS로 자동 전환하지 않는다. HTTPS를 쓰면 기존 CONNECT 443 경로를 이용한다. HTTP 예시의 접근키는 gateway까지 평문으로 전송되므로 전송 구간 선택 역시 운영자 설정이다.

allowlist는 기존대로 도메인과 그 label-boundary 하위 도메인을 허용한다. 전용 gateway 도메인을 사용하고 상위 공용 도메인이나 벤더 직접 도메인을 추가하지 않는다. 코드가 CLIProxyAPI라는 제품을 식별하거나 특정 주소를 하드코딩하지는 않는다.

파수꾼 판정:

- 구독 토큰은 T23 생산 경로에 입력하지 않는다. JANUS에는 구독 OAuth 수령·refresh·봉투 생성 경로가 없고, 기존 T15 전용 capability/smoke는 회귀 보존용으로만 남아 생산 launcher에서 사용하지 않는다. 따라서 저장소 전체에서 OAuth 문자열/옛 하네스까지 삭제됐다는 뜻은 아니다.
- 접근키의 허용 위치는 운영자 world-config, 전달 중 호스트 메모리·Podman 자식 env, 컨테이너 env와 그 표현인 `inspect.Config.Env`다. **inspect 전체 값 부재를 주장하지 않는다.** 그 밖의 argv·spawn metadata·audit·로그에는 키가 없어야 한다. env 값은 Podman argv에 넣지 않고 `--env NAME`으로 전달한다. stdout/stderr는 기존 stream redactor를 경유한다.
- 단위 파수꾼은 실제 키 대신 sentinel로 env 전달·argv/metadata 부재·분할 출력 redaction을 검사한다. 실제 컨테이너 env/inspect, session/replay, adapter argv, proxy audit는 [H]가 키 값을 화면이나 공유 로그에 출력하지 않고 값 포함 여부만 검사한다. env 예외는 agent에만 적용하고 proxy env에는 접근키가 없어야 한다. 구독 토큰은 어느 JANUS 산출물에도 없어야 한다.

**미검증, [H] smoke 필요:** 고정한 claude 이미지가 위 env만으로 로그인 인식 → `/v1/messages` 생성 → CLIProxyAPI 모델 응답을 받는지 확인한다. 이미지에 구독 로그인 파일을 넣거나 호스트 인증 디렉터리를 마운트하지 않는다. CLIProxyAPI 외 목적지는 거부되어야 한다. 이 검증이 완료되기 전에는 실 claude 종단 완료로 기록하지 않는다. tty 전달 문제는 T23에서 경로 자체가 제거되어 더 이상 차단 사항이 아니다.

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
