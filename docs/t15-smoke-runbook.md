# T15 사람 smoke runbook — [H] 실 토큰·macOS Podman VM

이 절차는 CI에 들어가지 않는다. 실 access token은 사람의 macOS를 떠나지
않으며, 값·환경 덤프·명령행·PR 로그에 기록하지 않는다. VM 결과는 Claude
인증·어댑터·승인 relay 보조 증거로만 사용하고, Linux rootless Podman
world 게이트의 증거로 재사용하지 않는다.

## 1. Podman machine 준비

```sh
podman machine init hx-t15
podman machine start hx-t15
podman info --format '{{.Host.Security.Rootless}} {{.Store.GraphDriverName}}'
```

`podman machine`이 실행되지 않거나 rootless 상태가 아니면 smoke를 성공으로
판정하지 않는다. 이 VM은 인증/어댑터 확인용이며 배포 커널을 대표하지 않는다.
실행 전 `podman machine inspect hx-t15` 결과에 token, 환경변수, 개인 설정
경로가 없는지 확인한다.

## 2. 실 token 주입

승인된 host credential provider에서 발급한 **단기 OAuth access token**만
사용한다. refresh token, Keychain 디렉터리, `ANTHROPIC_API_KEY`,
`claude setup-token`의 장기 token은 사용하지 않는다. 토큰은 명령행 인자나
파일에 쓰지 않고 프로세스 메모리에서 `world.NewSecretCapability`로 감싼 뒤
spawn 시 `CLAUDE_CODE_OAUTH_TOKEN` 환경변수 한 번으로만 전달한다.

운영자는 프롬프트 입력을 `read -s`로 받고 즉시 변수의 참조를 폐기한다. 실제
T15 world runner가 그 capability를 받는 승인된 진입점에서 다음을 확인한다:

```text
access token 입력(read -s) → host-only SecretCapability 생성
→ local-podman SpawnSpec에 비직렬화로 부착
→ Podman create 환경에 1회 주입
→ adapter argv/frame/metadata/raw/stderr에는 없음
```

복사해 실행할 수 있는 명령은 다음 한 줄뿐이다(셸 history에 token 값은 남지
않는다):

```sh
read -r -s HX_T15_ACCESS_TOKEN </dev/tty; export HX_T15_ACCESS_TOKEN; go test -tags t15smoke -count=1 -v -timeout 15m ./surfaces/hx -run '^TestT15HumanSmoke$'; status=$?; unset HX_T15_ACCESS_TOKEN; exit $status
```

실행 명령은 token 값을 포함하지 않는 형태로만 기록한다. `podman inspect`
출력 전체, `/proc/*/environ`, shell history, 화면 녹화에는 token을 남기지
않는다. smoke가 실패해도 API key·refresh·host 실행 폴백으로 우회하지 않는다.

## 3. 확인점

확인점을 다시 실행할 때의 명령은 다음과 같다. token을 매번 대화형으로 다시
입력하며, 실행 결과만 화면에서 확인하고 값을 복사하지 않는다.

```sh
read -r -s HX_T15_ACCESS_TOKEN </dev/tty; export HX_T15_ACCESS_TOKEN; go test -tags t15smoke -count=1 -v -timeout 15m ./surfaces/hx -run '^TestT15HumanSmoke$'; status=$?; unset HX_T15_ACCESS_TOKEN; exit $status
```

| 확인점 | 기대 결과 |
|---|---|
| (a) 인증·경계 | Claude가 container에서 기동하고 access token으로 인증한다. host adapter/process·approval socket·Podman socket은 agent에 없다. |
| (b) 승인 | native intent → `/run/hx/approve.sock` relay → allow/deny가 child span으로 기록된다. deny는 툴 효과가 없고 allow만 marker를 만든다. |
| (c) world 관측 | workspace lower는 불변이고 upper에 marker 변경이 남으며, 허용 egress는 proxy를 통하고 direct/IP literal은 dial 0 + deny audit이다. |
| (d) 비밀·종료 | durable spawn/event/raw/frame/stderr/오류에 token 원문이 없고, 정상 종료가 bounded cleanup으로 끝난다. |

각 확인점은 결과 전문 대신 성공/실패와 비밀을 제거한 진단만 기록한다.
`subagent/done`의 child span·status·result와 collector/egress record의 존재를
로그에서 확인하되 payload 원문에 credential을 복사하지 않는다.

## 4. 만료 token 음성 대조

전체 하네스에는 만료 전 예산 대조 음성 경로가 포함되어 있다. 이를 포함해
다시 실행하는 정확한 명령은 다음과 같다.

```sh
read -r -s HX_T15_ACCESS_TOKEN </dev/tty; export HX_T15_ACCESS_TOKEN; go test -tags t15smoke -count=1 -v -timeout 15m ./surfaces/hx -run '^TestT15HumanSmoke$'; status=$?; unset HX_T15_ACCESS_TOKEN; exit $status
```

실행 예산보다 짧은 만료 시각의 access token으로 한 번 더 실행한다. spawn
전에 `done`/container start가 발생하지 않고 명시적 deny가 나와야 하며, 실행
중 만료는 `subagent/done{status:error, result:"token expired"}`로 끝나야
한다. 재시도·refresh·API key 폴백은 실패다. 만료 token 값과 응답 본문은
기록하지 않는다.

## 5. 판정·기록

네 확인점과 만료 음성 대조가 모두 통과할 때만 [H]가 실 세션 보조 증거로
서명한다. 확인점 하나라도 실패하면 `BLOCKED.md`에 원인·단계·정리 결과를
기록하고 중지한다. 이 smoke는 T15의 access-token 스코프 축소 미충족을
해소하지 않으며, Linux CI의 rootless/native-overlay 증거를 대체하지 않는다.
