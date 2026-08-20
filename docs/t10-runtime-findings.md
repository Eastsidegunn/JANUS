# T10 실행 환경 조사 결과 — 2026-08-20

C안(통합 테스트는 Linux CI, 로컬은 Fake 백엔드) 확정을 위한 일회성 조사.
GitHub `ubuntu-latest` 러너에서 실행했고, 조사용 워크플로는 결과를 여기 옮긴 뒤
삭제했다(run 32270359463에 로그가 남아 있다).

러너: Ubuntu 24.04.4 LTS, 커널 6.17.0-1022-azure, x86_64, 메모리 15Gi,
디스크 145G 중 86G 여유.

## 1. 런타임 — 전부 사전 설치돼 있다

| 도구 | 버전 |
|---|---|
| podman | 5.8.4 |
| docker | 28.0.4 |
| buildah | 1.33.7 |
| skopeo | 1.13.3 |
| runc / crun | 1.4.3 / 1.28 |
| pasta | 2026_06_11 |
| fuse-overlayfs | 1.16 |

`slirp4netns`는 없다. pasta가 그 자리를 대체하므로 rootless 네트워킹에 문제
없다. **설치 단계가 필요 없다** — C안의 전제가 성립한다.

## 2. rootless (FR-SBX-01 SHOULD) — 성립

```
uid=1001 user=runner
/etc/subuid:runner:165536:65536
/etc/subgid:runner:165536:65536
max_user_namespaces = 63838
unprivileged_userns_clone = 1
podman rootless = true
```

subuid/subgid 65536 범위가 이미 배정돼 있고 user namespace가 열려 있다.
추가 설정 없이 rootless가 기본으로 동작한다.

## 3. overlayfs (FR-SBX-02) — 커널 네이티브

```
graphDriverName     = overlay
Native Overlay Diff = true
Backing Filesystem  = extfs
Supports d_type     = true
```

`fuse-overlayfs` 폴백이 아니라 **커널 overlayfs를 직접 쓴다.** rootless인데도
그렇다. 성능과 의미론 양쪽에서 유리하다.

### 반증 하나 — bind mount는 FR-SBX-02를 만족하지 않는다

조사에서 워크스페이스를 `-v host:/workspace:z`로 붙이고 컨테이너 안에서 파일을
수정해봤다. 결과:

```
## 호스트 원본 파일 내용
원본
컨테이너변경        ← 호스트 원본이 그대로 변경됐다
```

FR-SBX-02는 "원본은 lower, 변경은 upper에 격리"를 MUST로 요구한다. 단순 볼륨
마운트로는 성립하지 않는다. T10은 워크스페이스에 **명시적 overlay 마운트**를
구성해야 한다. 이 반증이 없었으면 볼륨 마운트로 통과했다고 착각했을 것이다.

## 4. 네트워크 (FR-SBX-03) — 기본이 허용이다

```
--network=none  → 차단됨 (기대대로)
기본 네트워크    → 통과   (기본 허용)
```

podman의 기본 bridge는 egress를 허용한다. 명세가 요구하는 "기본 차단"은 저절로
되지 않으며 명시적으로 구성해야 한다. 후보: `--network=none` + 강제 프록시
경유, 또는 전용 네트워크 + 필터. T10 착수 시 정한다.

## 5. 용량 실측

측정 방법: rootless graph root(`~/.local/share/containers/storage`)를 이미지
pull 전후로 `du -sm` 비교.

| 이미지 | 증분 | 누적 |
|---|---|---|
| alpine:3.20 | 8MB | 9MB |
| debian:bookworm-slim | 81MB | 90MB |
| ubuntu:24.04 | 84MB | 174MB |
| node:22-slim | 155MB | 329MB |

`podman system df`: 이미지 4개 321.5MB.

컨테이너 실행 후 스토리지 증가는 **0MB**였다(`--rm`으로 상위 계층이 함께
회수됨). 컨테이너 안에서 100MB를 썼는데도 그렇다 — 상위 계층은 컨테이너
수명에 묶인다.

### 판단

러너 여유 86GB 대비 이미지 수백MB는 무시할 수준이다. **용량은 C안의 제약이
아니다.**

다만 실제 샌드박스 이미지는 위보다 크다. FR-ADP-10에 따라 에이전트 본체가
컨테이너 안에서 돌아야 하는데, Claude Code의 경우 `claude` 실행 파일 하나가
299MB다(이 머신 실측). node 런타임까지 합치면 이미지가 450MB를 넘길 것으로
**추정**한다 — 실측이 아니다. 실제 이미지를 만들 때 다시 재야 한다.

## 6. C안 결론

설치할 것이 없고, rootless·overlayfs가 네이티브로 성립하며, 용량 제약이 없다.
로컬 macOS에서 VM을 경유해 검증하는 것보다 대표성도 높다.

남은 것은 구현 결정이고 차단은 아니다.

1. 워크스페이스 overlay 마운트 구성 (§3 반증)
2. egress 기본 차단 + 강제 프록시 (§4)
3. T9 승인 훅의 경계 통과 — `hxapprove`가 컨테이너 안, 부모는 호스트.
   Unix 소켓을 bind mount로 넘길지 다른 전송으로 바꿀지
4. 로컬 개발용 Fake 백엔드의 경계 (실제 구현으로 오인되지 않게 `Fake` 접두사)
