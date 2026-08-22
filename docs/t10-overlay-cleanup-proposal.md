# T10 rootless overlay cleanup 개정 제안

상태: 승인 대기. 이 문서는 PR #29 Linux 실물 run `32590121367`에서 남은
`overlay/work/work: permission denied`를 다룬다. cleanup 구현은 이 제안 승인
전에는 변경하지 않는다.

## 1. 관측과 선택

rootless native overlay는 `work/work`와 일부 upper entry를 subordinate UID로
만든다. host runner는 이들을 `lstat`할 수 있어 수집·whiteout 판별은 가능하지만,
디렉터리를 재귀 순회하며 삭제할 권한은 없다. 따라서 host `os.RemoveAll`은
`EPERM`이고, 소유자 필터나 실패 무시는 각각 삭제 유실과 자원 누수를 만든다.

선택지는 다음과 같다.

| 선택지 | 판단 | 실패 모드 |
|---|---|---|
| host `os.RemoveAll` 유지 | 탈락 | 실물 run에서 `work/work` EPERM |
| `chmod`/`chown` 후 host 삭제 | 탈락 | subordinate ownership을 host에서 바꾸지 못하며 `:U`와 같은 lower 변조 위험 |
| state 전체를 container에 mount해 삭제 | 탈락 | agent 또는 임의 helper에 host state 삭제 capability 노출 |
| `podman unshare rm -rf -- <검증된 target>` | 추천 | rootless storage와 같은 user namespace에서 subuid를 권위 있게 해석하며 신규 의존성이 없음 |

`podman unshare`는 rootless Podman 자체의 user namespace만 빌려 repo가 선택한
단일 경로를 삭제한다. shell, glob, 환경변수 치환은 사용하지 않고 argv로만
실행한다.

## 2. 경로 capability와 탈출 방지

삭제 API는 임의 문자열을 받지 않는다. backend가 보유한 `overlayLayout`과 비공개
target enum(`work`, 향후 `upper`)으로만 후보를 계산한다.

실행 직전에 다음을 모두 다시 검사하고 하나라도 어긋나면 Podman을 호출하지 않는다.

1. `stateRoot`는 Backend 생성 때 `EvalSymlinks`한 절대 경로이며 mode `0700`이다.
2. trace/span은 기존 정규식에 맞고 `stateDir`은 정확히
   `stateRoot/world/<trace>/<span>`이어야 한다.
3. target은 정확히 `stateDir/overlay/work` 또는 `stateDir/overlay/upper` 중
   선택된 하나여야 한다. `filepath.Clean` 결과와 원문이 같고 `filepath.Rel`로
   `stateRoot` 내부임을 재확인한다.
4. host가 접근 가능한 `stateRoot`부터 target까지의 각 경로 요소를 `lstat`해
   symlink와 비-directory ancestor를 거부한다. target 내부는 host가 순회하지
   않는다. 그 내부의 subuid 소유가 바로 user namespace cleanup을 쓰는 이유다.
5. 실행은 `podman unshare rm -rf -- <exact target>` 한 argv뿐이다. 성공 뒤
   host `lstat(target)`이 `ENOENT`인지 확인하며, 남아 있으면 cleanup 실패다.

이 검사는 T6의 정규화·scope 검사와 T10-3의 resolved lower/state-root 방어를
그대로 따른다. path 검증 실패, Podman 실패, 사후 잔존은 서로 구분된 오류로 남기고
조용히 성공 처리하지 않는다.

## 3. lifecycle과 collector ACK

`work`는 수집 증거가 아니므로 현재 순서를 유지한다.

```text
process stop/wait
→ effect drain/ACK
→ container/network 제거
→ userns에서 work 삭제
```

앞 단계 오류가 있어도 모든 단계를 시도하고 `errors.Join`으로 보존한다. caller
context가 취소돼도 cleanup은 bounded `context.WithoutCancel`로 계속한다.

`upper`는 다르게 취급한다. whiteout은 `lstat(mode+rdev)`로 읽을 수 있지만 삭제는
user namespace 권한이 필요하다는 비대칭을 계약으로 남긴다. T10 `Lease.Close`는
upper를 삭제하지 않는다. T11이 fsdiff를 durable 기록하고 collector ACK를 받은
뒤에만 lease-bound opaque ACK를 사용해 같은 내부 cleanup primitive의 `upper`
target을 호출할 수 있다. raw path를 받는 공개 삭제 API는 만들지 않는다.

따라서 cleanup 권위가 user namespace로 이동해도 “collector ACK 전에 upper 삭제
금지”는 변하지 않는다. T11 ACK API가 확정되기 전에는 upper cleanup 호출부를
선제 구현하지 않는다.

## 4. 검증 계획과 승인 요청

일회성 Ubuntu probe는 다음을 모두 확인해야 한다.

- rootless + native overlay 전제
- host `rm -rf`가 실제 `work/work`에서 실패
- `podman unshare rm -rf -- work` 성공과 사후 `ENOENT`
- nested delete로 만든 subuid-owned upper/whiteout에서 host 삭제 실패
- `podman unshare rm -rf -- upper` 성공과 사후 `ENOENT`
- state root 밖 sentinel 불변

프로브 성공 run ID는 이 절에 기록한다. 그 뒤 구현은 work target만 T10에 넣고,
기존 7항목 Linux gate를 하나도 줄이지 않고 다시 통과시킨다.

| 승인 항목 | 요청 |
|---|---|
| cleanup 권위 | work 삭제에 `podman unshare rm -rf -- <검증된 exact path>` 사용 |
| 경로 경계 | backend 계산 target enum + canonical containment + ancestor lstat, raw path API 금지 |
| 오류 의미론 | 모든 단계 시도, errors.Join, 사후 ENOENT 확인, 자원 누수 시 gate 실패 |
| upper | T10에서는 보존; T11 durable collector ACK 뒤 동일 primitive를 쓰는 별도 API 검토 |

