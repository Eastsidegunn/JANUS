# T10 process broker amendment — bounded shutdown and stream-consumer ownership

상태: **승인·구현·Linux 관통 검증 완료**. 대상: FR-LOG-09, FR-SBX-05, FR-ADP-10.

이 문서는 T10 lifecycle-stop 회귀에서 관측된 30초 정지를 타임아웃 숫자로
덮지 않고, 어디에서 대기가 생겼는지 식별하고 출력 스트림의 소유권을
명시하기 위한 개정안이다. 구현은 이 문서의 순서·분류를 따라야 하며 게이트
완화는 없다. 현재
Linux 관통 게이트의 실패는 다음과 같다.

| run | 관측 | 현재 결론 |
|---|---|---|
| `32826754125` | graceful stop이 약 30초 뒤 `output stream drain: context deadline exceeded` | 단계 식별 불가 |
| `32832407162`, `32832960325`, `32833303870` | graceful/stop-ignore 모두 stream drain timeout | 소비자 종료와 drain 종료가 결합됨 |
| `32833741349`, `32834630518`, `32835050107`, `32835632685`, `32836344535`, `32837057504` | 같은 증상 또는 진단 테스트의 독립적 불안정 | 수정 전 baseline; 원인 단계의 증거로 사용하지 않음 |

위 run들은 `chunks <-`, encoder `Write`, reader drain, attach 종료 대기 중 어느
지점이 30초를 소비했는지 기록하지 않는다. 따라서 이 개정안은 이 run들로 원인을
단정하지 않는다. 유효한 새 run은 아래의 단계 표식을 포함해야 한다.

## 1. 30초 정지의 단계 표식과 실물 판정

### 1.1 표식은 대기를 나눠서 기록한다

`drainAttach`의 단일 timeout을 다음의 불변 단계로 나눈다. 각 단계는 진입 시각,
종료 시각, lease/span, container ID, 정상 종료 여부를 가진 구조화된 진단을
남긴다. 단계 이름은 문자열을 이어 붙여 만들지 않고 고정된 enum으로 둔다.

| 단계 | 표식 | 의미 |
|---|---|---|
| `chunk-forward` | `chunks <- chunk` | attach reader가 읽은 chunk를 broker serializer로 넘기는 대기. 여기서 막히면 downstream 소비자가 없거나 serializer가 정지한 것이다. |
| `output-write` | output encoder `Write` | serializer가 output socket에 frame을 쓰는 대기. partial write와 peer EOF/write error를 별도 결과로 보존한다. |
| `reader-drain` | 두 attach reader의 `readersDone` 대기 | container가 종료된 뒤 이미 커널 버퍼에 있는 stdout/stderr를 읽어 끝내는 단계. container 종료 관측과 혼동하지 않는다. |
| `attach-exit` | `attach.Done()`/`ExitErr()` 관측 | attach transport subprocess의 종료. container exit 권위가 아니며, 단일 `podman wait` 결과와 별도로 기록한다. |
| `stream-end-write` | 최종 `stream_end` frame `Write` | 정상적인 종료 frame을 소비자에게 전달하는 마지막 쓰기. peer가 먼저 닫혔으면 consumer-gone으로 분류한다. |

사용자 요청의 두 핵심 지점은 각각 `chunk-forward`/`output-write` 묶음과
`attach-exit`로 반드시 구별한다. `reader-drain`은 attach 종료와 혼동되는
보조 표식이지만 생략하지 않는다. 한 단계에서 처음으로 종료되지 않은 경우를
`first_blocked_stage`로 원자적으로 보존하고, 이후 cleanup 단계의 오류가 이를
덮어쓰지 않게 한다.

### 1.2 실물 확인 방법

코드 변경 후 Linux/rootless 관통 게이트는 다음 두 모드를 각각 실행한다.

1. graceful `SIGTERM`을 처리하는 testagent (`lifecycle-stop`),
2. `SIGTERM`을 무시해 `SIGKILL` 경로를 밟는 testagent (`lifecycle-stop-ignore`).

각 모드에서 `first_blocked_stage`, 각 단계의 elapsed, `containerDone`,
`streamDone`, output peer 상태를 테스트 결과에 출력한다. `podman wait`가
먼저 끝난 경우와 output peer를 의도적으로 먼저 닫은 경우를 별도 케이스로
둔다. 새 run은 단계 표식이 없으면 성공 증거로 인정하지 않는다.

실패 run `32933127519`는 `reader-drain`, `32933493316`은 `attach-exit`,
`32933927934`는 ACK/terminal ordering 경합으로 단계별 원인을 특정했다. 수정
후 동일 SHA `62aa0ad`의 Linux run `32934687022` attempts 1, 2, 3, 4, 5가
각각 graceful/stop-ignore 관통 게이트와 전체 `make ci-linux`를 통과했다. 성공
경로에는 blocked stage가 없고, 테스트가 `containerDone`·`streamDone`·consumer
종말·cleanup bounded 및 runtime artifact 0을 단정한다. 이 5회 연속 증거로
amendment를 닫는다.
타임아웃을 늘리거나 실패 케이스를 제거하는 것은 허용하지 않는다.

참고로 GitHub Actions의 기본 `go test` 출력은 성공한 `t.Log`를 숨긴다. 따라서
성공 run의 단계 표식은 “blocked stage 없음”으로 해석하고, 각 단계 enum은 실제
대기 실패 시 오류에 보존된다. 성공 여부는 단계 문자열의 부재가 아니라
`containerDone`·`streamDone` 수렴, bounded cleanup, runtime artifact 0이라는
관통 단정으로 판정한다.

## 2. FR-LOG-09의 독자 경계

"이벤트 유실 없음"은 출력 독자가 존재하고 broker가 그 독자에게 backpressure를
전파할 때의 계약이다. 독자가 사라진 뒤에도 무한히 쓰기를 기다리는 것은 보존이
아니라 정지이므로 두 상태를 분리한다.

### 2.1 독자가 있는 경우

- output socket이 열려 있고 frame write가 진행 가능한 동안에는 bounded/unbuffered
  경로를 유지한다.
- `chunks`를 버리거나, 무제한 queue를 추가하거나, sampling/truncation으로
  green을 만들지 않는다.
- encoder write가 막히면 attach reader도 막혀 container agent의 출력까지
  backpressure가 전파된다. 이미 읽은 바이트는 `stream_end`까지 유실 없이
  전달되어야 한다.

### 2.2 독자가 없는 경우

output peer의 EOF, `net.ErrClosed`, `EPIPE`, 또는 최종 frame write의 확정된
connection error는 우선 `consumer-gone` 상태로 분류한다. 다만 graceful stop의
정상 종말에서는 이 상태가 fatal인지 아닌지를 **이미 전달된 출력의 존재와
종료 순서로 결정**한다.

다음 규칙을 채택한다(선택지 (a)).

- adapter가 native `done`을 durable하게 방출했고, 그 뒤 parent가 정상적인
  `Stop → Wait → Lease.Close`를 수행하는 중 output peer가 닫힌 경우: 더 전달할
  agent 출력이 없고 container exit가 이미 관측됐으면 `stream_end` 미전달은
  예상된 종말 상태다. 이를 `consumer-gone-after-done`으로 진단하지만
  `Close`의 성공을 실패로 바꾸지 않는다.
- adapter가 done을 방출하기 전, 아직 attach reader에 전달되지 않은 bytes가
  있거나 output write가 진행 중인 상태에서 peer가 닫힌 경우: `ErrStreamConsumerGone`
  fatal이다. 이 경우 실제 유실 가능성이 있으므로 lease는 실패한다.
- broker가 `stream_end`를 성공적으로 쓴 뒤의 EOF는 정상적인 peer close로
  간주한다. 이미 terminal frame이 소비자에게 도달했으므로 별도 오류가 아니다.

즉, "독자 없음"이라는 transport 사실만으로 graceful close를 실패시키지 않는다.
`doneSeen`, `containerDone`, `streamEnded`, `bytesPending`를 함께 기록해
두 상태를 재구성할 수 있어야 한다. 일반적인 adapter abort·protocol 오류·
consumer 이탈은 여전히 다음과 같은 별도 fatal 사유다.

- done 이전 유실 가능성을 정상 `stream_end`나 정상 EOF로 위장하지 않는다.
- 독자가 없어진 뒤 timeout을 기다리지 않고 broker가 즉시 stream/control
  정리와 container stop/kill을 시작한다.
- `first_blocked_stage=output-write` 또는 `stream-end-write`와
  `consumer=gone`을 진단에 보존한다.
- consumer-gone으로 인해 끝까지 전달할 수 없었던 출력이 있다는 사실은
  오류로 남긴다. 조용한 성공·평범한 종료가 아니다. 단, 위의
  `consumer-gone-after-done` 조건은 이 규칙의 예외인 비오류 종말이다.

반대로 container가 먼저 종료했고 output peer가 살아 있으면, 이미 버퍼된
유한한 바이트를 drain한 뒤 `stream_end`를 보내고 정상 종료할 수 있다. 이때도
`containerDone`은 attach EOF나 consumer 상태를 기다리지 않고 관측된다.

## 3. Shutdown 대기·취소 순서

현재 구조의 문제는 `Shutdown(ctx)`가 `containerDone`과 `streamDone`을
`ctx.Done()`에만 의존해 기다린 뒤 `b.cancel()`을 호출한다는 점이다. reader나
writer가 broker context 취소로만 풀리는 경우, 취소가 대기 뒤에 있어 30초 예산을
그대로 소진한다.

다음 순서를 계약으로 고친다.

1. `closing`과 최초 stop reason을 기록하고 listener를 닫는다.
2. stop 요청을 보낸다. stop이 실패하면 kill을 요청하되, 두 경로 모두 **단일
   `podman wait`** 관측 goroutine을 깨우는 수단으로만 사용한다.
3. `containerDone`을 관측한다. 이 시점 이후 container는 새 stdout/stderr를
   생산할 수 없다. 따라서 남은 drain은 attach pipe/kernel buffer에 이미 있는
   유한한 바이트뿐이다.
4. 즉시 broker drain context를 cancel하고, 필요하면 attach pipe를 닫아 reader가
   `os.ErrClosed`로 끝나게 한다. 이 취소는 `containerDone` 뒤에만 수행해
   정상적으로 이미 생산된 버퍼를 먼저 읽을 기회를 보존한다.
5. output peer가 살아 있으면 reader drain과 최종 `stream_end`를 완료한다.
   peer가 없으면 `ErrStreamConsumerGone`으로 즉시 종료한다. 두 경우 모두
   `streamDone`은 context deadline만으로 끝나지 않는다.
6. output/control connection, attach/wait subprocess pipe를 닫고 goroutine을
   join한다. cleanup context는 호출자 context와 분리된 bounded context를
   사용하되, 그 예산 초과는 `cleanup stage=<name>`으로 표면화한다.
7. 마지막으로 state root/network/container 정리를 실행하고 모든 단계 오류를
   `errors.Join`으로 반환한다.

즉, `containerDone` 이전에 cancel에만 의존하는 drain 대기를 두지 않는다.
컨테이너가 살아 있는 동안의 backpressure는 유지하지만, 종료가 관측된 뒤에는
유한 drain 또는 명시적인 consumer-gone으로 수렴해야 한다.

## 4. 어댑터 소켓 종료 규약

### 4.1 현재 블록의 원인

현재 adapter는 `defer process.Close()`를 `Run` 반환 시점까지 보류한다. native
stdout scan이 끝난 뒤 adapter는 먼저 `outputDone`(broker의 `stream_end`를
기다리는 goroutine)을 기다리고, 그 다음 `process.Wait`를 수행한다. broker는
동시에 최종 `stream_end`를 output socket에 쓰려 한다. 따라서 adapter가 output
socket을 닫아 broker의 write를 즉시 실패시키는 경로가 없고, peer 종료와 broker
drain 완료가 서로를 기다리는 순환이 생길 수 있다. 현재 remote failure의
`stream_end: write ... use of closed network connection`은 이 순환이 timeout
이후에 드러난 결과이지, 30초의 원인 단계를 증명하지 않는다.

### 4.2 명시적 종료 규약

process broker protocol에 다음 transport 규약을 추가한다(contracts/ JSON
schema 변경은 아니다).

| 상태 | adapter 동작 | broker 의미 |
|---|---|---|
| 정상 native done + broker `stream_end` 수신 | output 연결을 즉시 close, control에서 exit 관측 후 close | `stream_end`가 소비자에게 전달된 정상 종료 |
| native parser/command/contract 오류 | 가능한 경우 deterministic done을 먼저 기록한 뒤 output 연결을 close; control을 닫고 반환 | peer EOF는 `consumer-gone`이 아니라 `adapter-abort(reason)`로 진단 |
| parent stop/cancel | stop command를 한 번 보내고 output/control을 즉시 close; broker가 containerDone을 관측 | stop reason이 권위이며 남은 pending은 deny/정리 |
| broker가 먼저 `ErrStreamConsumerGone`을 알림 | adapter는 더 쓰지 않고 모든 endpoint를 close | 재연결 금지, 새 lease만 재시도 |

adapter는 `stream_end` 수신을 무한히 기다린 뒤 defer에 기대지 않는다. output
reader가 EOF/error를 관측하면 `stdoutW`를 닫고, process output endpoint를 명시적으로
닫아 broker writer가 즉시 결과를 받게 한다. control endpoint도 같은 terminal
state에서 닫는다. broker는 output EOF를 정상 종료로 승격하지 않고, 마지막으로
관측한 frame과 adapter terminal reason에 따라 `consumer-gone`/`adapter-abort`를
구분한다.

이 규약은 adapter가 나간 뒤 남은 kernel buffer를 무한히 보존한다고 약속하지
않는다. adapter가 독자로서 endpoint를 닫은 순간 FR-LOG-09의 "독자 있음" 계약은
끝나며, broker는 유실 가능성이 있는 종료를 명시적인 오류로 남기고 lease를
실패시킨다.

## 5. 오류·자원 고갈 의미론

- handler/parser 계약 위반은 process broker stream 오류보다 우선한다.
- consumer-gone은 평범한 `io.EOF`가 아니라 fatal sentinel로 구분한다.
- frame/queue/connection 상한 초과는 `ErrProcessBrokerFatal` 아래의
  `ErrResourceExhausted`로 기록한다. 4단계의 503/403 구분과 같은 원칙이다.
- 모든 실패 경로에서 stop/kill → 단일 wait → stream/control close → cleanup을
  끝까지 수행한다. 중간 오류 때문에 자원을 남기지 않는다.
- 채널과 goroutine 대기는 각 단계별 2초 상한을 갖고, 상한에 도달하면
  `stage=<enum>`과 `first_blocked_stage`를 포함한 오류를 반환한다. 타임아웃은
  원인 분류의 마지막 안전망이지 정상 종료 방법이 아니다.

## 6. 승인·검증 요청

| 항목 | 결정 | 구현/검증 조건 |
|---|---|---|
| 30초 원인 식별 | 단계 enum과 first-blocked-stage를 추가 | Linux 실물 run에 `chunk-forward`, `output-write`, `reader-drain`, `attach-exit`, `stream-end-write` 결과와 run ID 포함 |
| FR-LOG-09 독자 경계 | 독자 있음은 lossless backpressure; done 이후 정상 close는 `consumer-gone-after-done`, done 이전 이탈은 `ErrStreamConsumerGone` | doneSeen/containerDone/streamEnded/bytesPending를 기록하고, 정상 EOF 위장·무한 대기는 금지 |
| Shutdown 순서 | containerDone 관측 후 drain context 취소, 유한 buffer drain 또는 consumer-gone | graceful/stop-ignore 모두 container·stream·cleanup bounded |
| socket 규약 | adapter terminal state에서 output/control 명시적 close | adapter의 `outputDone` 선행 대기 순환 제거; 재연결 금지 |
| 회귀 보존 | stop-ignore 관통 모드 유지 | graceful와 stop-ignore 각각 5회 연속 Linux green |

이 amendment가 닫히려면 코드·테스트 변경 후 새 Linux run ID 다섯 세트를 PR에
첨부해야 한다. 각 세트에는 두 모드의 단계 표식, `containerDone`/`streamDone`,
consumer 상태, cleanup 결과가 있어야 한다. 현재 baseline run은 단계 표식이 없어
이 조건을 충족하지 않는다.

## 7. 범위 밖

- Podman 네트워크·overlay 자체의 신규 설계는 다루지 않는다.
- Claude Code T15 sandbox 경로는 다루지 않는다.
- contracts/ JSON schema와 fixtures는 수정하지 않는다.
- 타임아웃을 늘려 실패를 green으로 만드는 변경, 게이트 삭제·skip, 출력
  truncation/drop, 재연결을 통한 exactly-once 가장은 금지한다.
