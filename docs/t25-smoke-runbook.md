# T24+T25+D-track 통합 smoke 런북 — 실 claude 정상경로·다중 턴·정지점 포착 (명세 소유자 전용)

## 변수 정의

각 환경에 맞게 아래 경로와 주소를 설정한다. 예시 값은 일반적인 개발 환경이며 저장소에서 제공하는 값이 아니다.

```sh
STAGING_DIR="${STAGING_DIR:-/tmp}"
JANUS_INSTALL_DIR="${JANUS_INSTALL_DIR:-$HOME/janus-bin}"
SMOKE_ROOT="${SMOKE_ROOT:-$HOME/janus-smoke}"
HX_BIN="${HX_BIN:-$SMOKE_ROOT/bin/hx}"
SMOKE_BIN="${SMOKE_BIN:-$SMOKE_ROOT/bin/smoke}"
SMOKE="$SMOKE_ROOT"
```

> **명세 소유자 전용.** 실 토큰·외부 효과(구독 사용량)이므로 사람이 직접 수행한다(결정 10번).
> 에이전트에 위임하지 않는다 — 배포 호스트 접근과 실 토큰 단계는 운영자가 직접 수행하며
> 권한 통제를 우회하지 않는다.
> 한 번의 smoke로 세 목적을 동시에 얻는다: **A** T24 정상경로 + T25 다중 턴 실증,
> **B** D-track 정지점 포착(SIGQUIT 고루틴 덤프).
> 기존 환경 준비(hx 빌드·PROFILE·WORLD·SMOKE·SOCK·CLIProxyAPI)는
> `docs/t17-20-smoke-runbook.md` §1~§2를 그대로 쓴다. 이 문서는 그 위의 차분만.

## 0. 전제·설치

- 바이너리: T25가 머지된 main의 클린 빌드 + (배포 호스트가 Podman 3.x일 때만) 호환 패치(오케스트레이터가 `"$STAGING_DIR"/*.new`로
  준비: hx·claudecode·worldadapter 3종). `$JANUS_INSTALL_DIR`에 백업(`.t24bak`) 후 설치.
- **bypassPermissions 패치는 이 런북에 불필요하다.** §A는 도구를 안 쓰고, §B는
  hook 유무와 무관하게 정지가 재현된다(Rhizome 대조 결과). 해당 패치 적용 여부는
  별도 통치 결정이며 이 런북의 판정에 영향을 주지 않는다.
- `hx run`은 **stderr를 파일로 남긴다**(아래 모든 명령에 `2>>$SMOKE/hx.stderr.log`).
  어댑터 stderr는 hx가 그대로 상속하므로(AdapterStderr=os.Stderr) 덤프가 한 파일에 모인다.
- 소켓 op는 NDJSON 1줄 요청 → 1줄 응답. 보조 도구: `socat`(또는 `nc -U`).
  ```sh
  op() { printf '%s\n' "$1" | socat - UNIX-CONNECT:"$SOCK"; }
  ```
- Rhizome `smoke start`는 `session_mode`를 못 넣는다. 다중 턴은 request.json을 직접
  써서 `hx run --request`로 띄운다(§A-2).

## A. T24 정상경로 + T25 다중 턴 (도구 없는 과제)

### A-1. 템플릿 확보 겸 T24 정상경로 실증 (oneshot)
```sh
"$SMOKE_BIN" start -journal "$J" -exec "$EXEC" -hx "$HX_BIN" \
  -profile "$PROFILE" -accept-root "$SMOKE/accept" -world-config "$WORLD" \
  -session-db "$SESSION" -approval-endpoint "$SOCK" \
  -request-dir "$SMOKE/req" \
  -operation-id op-t24a -scope "$SCOPE" -fingerprint fp-t24a \
  -adapter claudecode -workspace-ref /workspace \
  -instruction "2+2는? 숫자만 답하라." \
  -profile-id <PROFILE id> -profile-hash "$PHASH" 2>>$SMOKE/hx.stderr.log
```
- `-request-dir`로 생성된 request.json이 `$SMOKE/req/`에 남는다 → §A-2 템플릿.
- 세션 종료 후 `$HX_BIN replay --session "<accepted의 session_ref.session_db>"`:
  **판정 A-1 = turns≥1 · messages≥1 · done ok**, CLIProxyAPI 로그에 요청 1건.
  (T24 완료 기준의 정상경로 종단 — 여기서 실패하면 T25 진행 중단, 전문 리뷰어에게.)

### A-2. 다중 턴 request.json
`$SMOKE/req/request.json`을 `$SMOKE/multi.json`으로 복제하고 **5곳만** 바꾼다:
```
"operation_id":        "op-t25a"
"idempotency_key":     (새 값 — 같은 key면 이전 세션 재반환)
"request_fingerprint": (새 64자리 소문자 hex, 예: $(head -c32 /dev/urandom | sha256sum | cut -c1-64))
"rhizome_execution_id":(새 값)
"session_mode":        "multiturn"        ← 추가
```
나머지(profile_id·profile_hash·budget·adapter_id·workspace_ref·instruction)는 그대로.
instruction은 "2+2는? 숫자만 답하라."로 유지(도구 유발 금지).

### A-3. 시작 (터미널 1 — 세션이 stop까지 살아 있음)
```sh
"$HX_BIN" run --request "$SMOKE/multi.json" --profile "$PROFILE" \
  --accept-root "$SMOKE/accept" --world-config "$WORLD" \
  --approval-endpoint "$SOCK" 2>>$SMOKE/hx.stderr.log
```
stdout의 accepted NDJSON에서 `trace_id`(=session_id)와 `session_ref.session_db`를 기록:
```sh
TRACE=<trace_id>; DB=<session_db>
```

### A-4. 첫 턴 관측 (터미널 2)
```sh
op '{"op":"events_tail","session_id":"'"$TRACE"'","from_seq":0}'
```
기대: `events[]`에 subagent/ready → subagent/message("4") → subagent/usage, `session.status`=`running`,
`next_from_seq`=N. (done이 **없어야** 정상 — multiturn은 첫 result 후 stdin을 열어둔다.)

### A-5. 후속 메시지 주입 — **T25 핵심 실증**
```sh
op '{"op":"send_message","session_id":"'"$TRACE"'","text":"그러면 3+3은? 숫자만 답하라."}'
# 기대: {"status":"message_accepted","message_seq":M}
op '{"op":"events_tail","session_id":"'"$TRACE"'","from_seq":'"$M"'}'
```
**판정 A-5 = seq M에 user/message(주입문) → 이어서 subagent/message("6") → subagent/usage, seq 연속.**
이것이 성립하면 실 claude에서 (가정 1) `-p` + `--input-format stream-json` stdin 후속 입력 수용,
(가정 2) 턴 경계 init 재방출 처리 — 두 미실증 가정이 동시에 실증된다.

실패 양상별 해석(둘 다 `hx.stderr.log` 전문과 함께 리뷰어에게):
- user/message는 기록됐는데 이후 이벤트가 영영 없음 → 가정 1 깨짐(claude가 -p 모드에서
  stdin 후속 턴을 안 받음). send_message 응답이 `DELIVERY_FAILED`면 전달 단계 실패.
- 어댑터 오류로 세션이 done{error}("init" 언급) → 가정 2 깨짐(턴 경계 init 처리 불일치).

### A-6. 중단 (T19 재사용) 및 종료 확인
```sh
op '{"op":"stop","trace_id":"'"$TRACE"'","stop_id":"stop-t25a","reason":"user"}'
# 기대: status stop_accepted
op '{"op":"events_tail","session_id":"'"$TRACE"'","from_seq":0}'
# 기대: 마지막에 subagent/done status=stopped, session.status=exited
```
터미널 1의 `hx run`이 종료되고, `$HX_BIN replay --session "$DB"`에서 turns≥2·messages≥2.
CLIProxyAPI 로그에 요청 **2건**(턴별). `podman ps -a`에 잔존 컨테이너 0.

## B. D-track 정지점 포착 (도구 과제, oneshot으로 충분)

### B-1. 정지 재현
A-1과 같은 `smoke start`를 **instruction만 바꿔** 실행(새 operation-id/fingerprint):
```
-instruction "Bash 도구로 ls /workspace 를 실행하고 결과를 보고하라"
```
기대 증상: ready 이후 무음(`$HX_BIN replay`에 message/tool_call/approval_request 없음), stderr 무오류.
**승인 요청이 소켓에 올라오면(= 정지 아님) 그냥 deny하고 B를 종료한다 — 그 경우 D-track은
이 빌드에서 재현되지 않는 것이며 그 사실 자체가 보고 대상.**

### B-2. 정지 중 포착 (타임아웃 전에, 다른 터미널)
```sh
ss -xp | grep -i approval > $SMOKE/ss.txt          # Recv-Q: 연결됐는데 accept 안 된 큐
pgrep -af 'claudecode' ; pgrep -af 'hx run'       # 호스트 어댑터 pid, hx pid 확인
kill -QUIT <claudecode pid>                        # Go 런타임 전 고루틴 스택 → stderr
kill -QUIT <hx pid>
```
두 덤프는 `$SMOKE/hx.stderr.log`에 쌓인다(SIGQUIT는 덤프 후 프로세스를 종료시킨다 —
진단 목적이므로 정상). 이후 `podman ps -a`로 컨테이너 잔존 확인·정리.

### B-3. 전달
`hx.stderr.log`(덤프 2개)와 `ss.txt`를 리뷰어에게(Rhizome 릴레이 가능). 공유 전
`grep -c "<CLIProxyAPI 접근키>" hx.stderr.log`가 0인지 확인(스택 덤프에 env는 안 찍히지만
습관으로). 리뷰어가 덤프에서 registerIntent 대기 지점(dial/Encode/Decode)·브로커
handleHost 고루틴 존재 여부·drainAttach forwarder 상태를 지목한다.

## 성공 판정 요약

| 항목 | 판정 기준 |
|---|---|
| A-1 T24 정상경로 | oneshot 세션 turns≥1·messages≥1·done ok, CLIProxyAPI 요청 1건 |
| A-5 T25 다중 턴 | user/message(seq M) 뒤 두 번째 subagent/message·usage, seq 연속 |
| A-6 중단·종료 | stop_accepted → done stopped, replay turns≥2, 컨테이너 잔존 0 |
| B D-track | 정지 재현 + 덤프 2개 + ss 출력 확보 (또는 "재현 안 됨" 보고) |

어느 단계든 실패하면 멈추고 출력 전문(stdout NDJSON·hx.stderr.log)을 리뷰어에게. 모든
단계는 새 operation-id/idempotency_key/fingerprint로 재실행 안전.
