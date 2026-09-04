# T15 제안서 — Claude Code 에이전트의 sandbox 실행

상태: 구현 전 제안. 결정 2026-09-04: 스코프 미축소는 문서화된 부분 충족으로
수용하고, 실 세션은 macOS Podman VM에서 인증·adapter 범위로 수행한다. 이 문서는
`contracts/`·`fixtures/`·코드·CI 자격증명을 변경하지 않는다. 대상은
FR-ADP-10, FR-SBX-01(Claude Code 경로), FR-SBX-04이며,
Claude Code 샌드박스 실행만 다룬다. T10의 컨테이너 test subagent가 이미 증명한
world 기판, T10 process broker·approval relay·overlay·egress·spawn receipt를
재사용하고 새 lifecycle 구현을 만들지 않는다.

## 확정된 전제와 남은 판단

`docs/t10-scope-determination.md §6`의 2026-09-02 [H] 실측으로 착수 조건은
충족됐다.

- OAuth access token만 주입한 격리 환경에서 Claude 2.1.252가 인증했고, 변조한
  token은 `401 OAuth access token is invalid`로 즉시 실패했다. 주변 Keychain이나
  환경변수로 조용히 폴백하지 않았다.
- `node:22-slim` 컨테이너에서 같은 Claude 버전이 자격증명 없이 인증 관문
  (`Not logged in · Please run /login`)까지 기동했다(CI run `33536620006`).
- access token은 계정 전체 권한이라 **스코프 축소는 아직 미충족**이다. 이는
  구현자가 임의로 완화할 항목이 아니며 아래 승인 표에서 별도의 [H] 판단으로
  남긴다.

T10의 `world_backend:"none"`는 호스트 `procgroup` 경로로 그대로 유지한다.
T15는 `local-podman` 분기에서만 Claude 본체를 컨테이너 안으로 옮긴다.

## Q1. 자격증명 (FR-SBX-04)

### 선택지

| 안 | 선택 | 실패 모드 |
|---|---|---|
| 장기 `ANTHROPIC_API_KEY`를 환경변수로 주입 | 탈락 | FR-SBX-04 MUST NOT 위반이며 회수·범위 제한이 불가능하다. T9에서 미승인됐다. |
| refresh token/Keychain 디렉터리를 mount | 탈락 | 장기 자격증명이 컨테이너 경계를 넘고 세션·호스트 상태가 노출된다. |
| Bedrock/Vertex STS | 보류 | 이 환경에는 공급자 접근이 없어 [H] 실측할 수 없다. 동작한다고 지어내지 않는다. |
| **호스트가 보유한 OAuth access token 한 개를 spawn 시 env로 주입** | **선택** | access token이 만료되거나 계정 권한이 허용하지 않으면 Claude가 인증 오류로 종료한다. 이를 성공으로 폴백하지 않는다. |

### 선택안과 비밀 경계

호스트의 자격증명 획득기는 refresh token을 보관하고, spawn 직전에 단기 access
token과 만료 시각만 host-only `SpawnSpec`의 비직렬화 secret capability로 넘긴다.
world가 컨테이너 생성 시 `CLAUDE_CODE_OAUTH_TOKEN` 환경변수로 한 번 주입하고,
process broker frame·명령 인자·mount·approval relay에는 token을 넣지 않는다.
컨테이너에는 refresh token, Keychain, `CLAUDE_CONFIG_DIR`, 호스트 소켓, Podman
소켓을 전달하지 않는다. host adapter와 writer는 token을 읽거나 보관하지 않고,
`SpawnMetadata`, `subagent/spawn`, 모든 event `raw`, stderr 진단과 OTel 속성에도
secret 필드가 없도록 타입과 redaction 경로를 유지한다.

token 자체를 명령행에 넣지 않으므로 `/proc/*/cmdline`·process broker 로그에는
남지 않는다. 컨테이너 내부 프로세스가 자기 환경을 읽을 수 있다는 것은 Claude
실행에 필요한 입력이지 보안 경계로 간주하지 않는다. 호스트에서 token이
`podman inspect` 결과나 오류 문자열로 재출력되지 않도록, inspect는 image
metadata만 읽고 환경을 기록하지 않는 별도 경로로 둔다. 회귀는 다음을 모두
검색한다: durable spawn payload/metadata, native raw, process control/output
frame, stderr, writer 입력, 실패 오류 문자열. `<redacted>` 표식과 원문 부재를
동시에 단정하며 실제 token 값은 fixture·CI secret에 넣지 않는다.

access token의 잔여 수명이 spawn 예산보다 짧으면 world가 container를 만들기
전에 명시적 deny로 끝낸다(예산 + clock-skew 여유를 포함한 비교). 실행 중 만료는
Claude의 인증 오류/비정상 종료를 process broker가 관측하고 adapter가
`subagent/done{status:error}`와 결정적인 `token expired` 사유를 합성한다. token,
HTTP response body, credential header는 결과·raw·stderr에 포함하지 않는다.
재시도·refresh·API key fallback은 만들지 않는다. 종료 상태와 로그의 원인이
구분되어야 하며, token 만료를 정상 `ok`로 바꾸지 않는다.

**스코프 판단은 별도 승인 대상이다.** access token은 계정 전체 권한이므로
FR-SBX-04의 “스코프를 좁힌 단기 토큰” MUST 중 스코프 축은 아직 미충족이다.
명세 소유자가 수용하거나 FR-SBX-04를 개정하기 전에는 T15가 해당 FR을
완료했다고 표시하지 않는다. 구현은 단기성·장기 token 반입 금지만 검증하고
스코프 충족을 가장하지 않는다.

## Q2. 어댑터 분할과 승인 경계 (FR-ADP-10)

### 선택지

| 안 | 선택 | 실패 모드 |
|---|---|---|
| 기존 `procgroup.Start`로 host에서 Claude 실행 | 탈락 | 실제 agent가 host에 남아 FR-SBX-01을 위반한다. T10 통합 테스트의 testagent 증거로 대체할 수 없다. |
| host adapter binary를 container에 mount하고 내부에서 실행 | 탈락 | agent가 adapter 코드·환경·승인 capability를 변조할 수 있어 FR-ADP-10 경계를 깨뜨린다. |
| container에 Claude 본체만 두고 host ProcessEndpoint로 stdio 중계 | **선택** | process broker 연결/순서/cleanup이 실패하면 spawn은 오류로 끝나며 host 실행으로 폴백하지 않는다. |

`claudecode`는 `world_backend:"local-podman"`에서 `world.AgentDescriptor`의
host-only `ProcessEndpoint`를 통해 native stdin/stdout/stderr를 말한다. T7의
procgroup 경로와 null adapter는 `none` 분기로 그대로 보존한다. endpoint 명목
타입을 `ApprovalEndpoint`와 섞지 않고, 재연결·Podman socket 전달도 금지한다.

컨테이너 이미지에는 Claude 실행에 필요한 고정 버전과 testable hook helper만
넣는다. host adapter 실행 파일, 작업 디렉터리, approval host socket, process
broker host socket은 mount하지 않는다. agent가 볼 수 있는 유일한 승인 통로는
T10-5의 request-only `/run/hx/approve.sock` relay다. `hxapprove`는 그 relay에
hook 원본 한 종류만 전달하고, relay 프로토콜·one-shot 상관·durable
policy/decision 선행 규칙을 변경하지 않는다. token/HMAC을 신뢰 경계로 삼지
않으며, relay가 허용하지 않는 adapter 명령·response 주입 수단을 만들지 않는다.

실제 `tool_use`가 native stream에 나타나기 전에 adapter가 intent를 등록하고,
hook은 relay로 들어와 동일 `(call_id, name, canonical args)`와 현재 lease/span을
검증한 뒤에만 대기 중 결정에 도달한다. manual 정책에서 relay/Decider가
관측되지 않으면 allow하지 않고 명시적 `done/error`로 끝낸다. 명시적 auto만
T9 규칙대로 자동 허용한다. container 내부 process가 relay를 직접 위조해도
host ledger·lease·one-shot 검사를 통과할 수 없고, 위조 시도는 durable deny와
native rejected/done으로 재구성된다.

## Q3. Claude 컨테이너 이미지

### 선택지

| 안 | 선택 | 실패 모드 |
|---|---|---|
| 변동 tag(`node:latest`, `claude@latest`) | 탈락 | 재현성과 native stream/인증 동작이 보장되지 않는다. |
| 외부에서 미리 만든 불투명 이미지 pull | 탈락 | 공급망·digest 출처를 검증할 수 없고 CI 결과를 재현할 수 없다. |
| **CI에서 pinned Node base + exact Claude version으로 재현 빌드** | **선택** | npm/base image pull·빌드 실패는 `INFRASTRUCTURE`로 표면화하고 실행/로그 검증 실패와 구분한다. |

구성은 `node:22-slim`의 **digest 고정 base** 위에 Claude Code `2.1.252`를
정확한 npm version으로 설치하고, repo에서 빌드한 정적 `hxapprove`/필요한
런처만 복사한다. `latest`, 범위, floating npm dependency를 사용하지 않는다.
최종 이미지는 CI build 후 `podman image inspect --format '{{.Digest}}'`로
확정하고 spawn metadata에는 digest만 기록한다. tag는 build convenience일 뿐
실행 참조가 아니다.

현재 macOS에는 Podman이 없어 **450MB라는 추정치를 실측하지 않았다.** 구현
단계의 Linux 프로브가 pinned base pull, npm 설치, 이미지 build를 수행한 뒤
`Size`(bytes)와 압축 전/후 artifact 크기를 출력·PR에 기록한다. 이 수치가
확정되기 전에는 용량을 완료 근거로 주장하지 않는다. 예상치와 실제 digest를
섞지 않는다.

npm registry와 Node base registry pull은 T13의 “외부 비의존 artifact service”와
다른 종류의 의존성이다. Claude 본체를 공급하는 외부 registry 없이는 이 태스크
자체를 수행할 수 있으므로, pull을 숨기지 않고 CI의 인프라 단계로 격리한다.
pull 실패는 `INFRASTRUCTURE: claude image pull/build failed`로 종료하며,
Claude 인증·broker·sandbox assertion 실패와 같은 오류로 위장하지 않는다.
정확성은 digest와 lockfile로 보장하고, CI layer cache는 선택적 성능 최적화일
뿐 캐시 miss를 실패로 취급하지 않는다. 외부 registry가 불능인 경우 테스트를
skip하거나 다른 tag로 우회하지 않는다.

## Q4. 검증 전략과 증거 경계

완전한 컨테이너 + 실 token + 실 Claude 세션은 현재 CI에 넣을 수 없다. public
repository CI에 token을 저장·주입하지 않으며, macOS에는 Podman runtime이 없다.
따라서 무자격증명 CI와 [H] 실 세션을 분리하고, 한쪽의 결과를 다른 쪽의
완료 근거로 합치지 않는다.

### CI (자격증명 없음)

Linux/rootless/native-overlay 조건과 Podman이 없으면 skip이 아니라
`INFRASTRUCTURE` 실패다. CI는 pinned Claude image를 build하고 token 없이
인증 관문까지 기동시켜 `Not logged in` 경로를 확인한다. 이어 host
`claudecode` adapter가 실제 container process broker와 연결되어 다음을
단정한다.

1. process start가 durable spawn ACK 뒤이고, agent PID/adapter hash가 각각
   container/host 경계에 있다.
2. container 내부 stdio가 ProcessEndpoint를 통과하며 host `procgroup` 폴백이
   발생하지 않는다. abnormal exit·stop·orphan descendant도 T10 bounded
   lifecycle로 끝난다.
3. T10 overlay upper에 container 작업 결과가 보이고 lower는 불변이다.
4. agent의 direct external/IP literal은 dial 0 + deny audit이고, 허용 도메인은
   proxy를 통해서만 allow audit된다.
5. `/run/hx/approve.sock` relay에서 native intent→hook approval→rejected/allow
   순서와 child span 귀속을 확인한다. host process/approval socket 및 Podman
   socket은 agent에 없다.
6. token 없는 Claude의 인증 실패가 `done{error}`로 관측되고, 그 오류/이미지
   metadata/raw 어디에도 credential이 나타나지 않는다.

이 층은 Claude API 호출 성공이나 실제 tool side effect를 주장하지 않는다. 실
   token 없는 세션에서 Claude가 인증 관문까지 도달하지 못하면 이는 명시적
   CI 검증 실패이지 자동 skip이 아니다.

### [H] 실 세션

| 실행 위치 | 장점 | 한계/결정 |
|---|---|---|
| macOS Podman VM | 로컬 access token과 Claude 계정으로 빠른 인증·hook 확인 가능 | Linux kernel/overlay/network 배포 대상을 대표하지 않는다. T10 world gate의 증거로 재사용하지 않고, Claude 인증/adapter smoke에 한정한다. **선택** |
| Linux box/Ubuntu runner의 일회성 수동 세션 | 실제 rootless Podman·overlay·proxy·relay와 Claude를 같은 커널에서 검증 | token을 원격 머신으로 반출하는 비용이 있고, 커널 기제는 이미 무자격증명 CI에서 검증한다. 선택하지 않는다. |
| 실 세션 미실행 | 자격증명 반출 위험 없음 | §8-2의 실 tool call/child span, FR-SBX-01 Claude 경로, FR-SBX-04 전체를 검증할 수 없다. 검증 공백으로 남긴다. |

사람 smoke는 macOS Podman VM에서 (a) token이 실제로 짧은 access token인지, (b) refresh token/API
key가 없는 격리 환경인지, (c) tool allow/deny가 relay를 통과하는지, (d) marker와
upper diff 및 egress audit이 함께 남는지를 확인한다. 커맨드·결과 전문·token
값은 분리하며 token 자체는 PR/로그에 기록하지 않는다. 만료 테스트는 변조·만료
token으로 `done/error`를 확인하고 성공으로 폴백하지 않는다.

## Q5. 종결과 추적

구현은 다음을 한꺼번에 완료했다고 표시하지 않는다.

- `FR-ADP-10`: `local-podman` Claude 본체가 container 안에 있고, host adapter와
  process/approval capability가 변조 불가 경계로 분리됐음을 CI 및 [H] smoke로
  증명한다. `none`의 기존 host procgroup/null 경로는 계속 유효해야 한다.
- `FR-SBX-01 (Claude 경로)`: 실제 Claude agent PID가 container 내부이고,
  workspace upper 변경과 proxy egress가 관측된 뒤에만 완료한다. T10의 testagent
  결과를 이 행의 대체 근거로 쓰지 않는다.
- `FR-SBX-04`: refresh/장기 credential을 반입하지 않고 access token 만료를
  fail-closed로 관측하는 부분은 구현할 수 있지만, access token의 **계정 전체
  scope** 때문에 “스코프를 좁힌” MUST는 [H]가 수용하거나 명세를 개정하기 전까지
  미충족으로 유지한다.

명세 §8-2는 “Claude Code와 Codex 어댑터가 골든 픽스처 테스트를 통과하고,
실 세션에서 자식 툴 콜이 child span으로 기록된다”고 요구한다. Codex 골든과
T14 OTel 관통은 이 제안의 입력이지만, Claude의 실제 tool call/child span은
위 [H] 실 세션에서만 충족된다. 실 세션이 없으면 traceability에
`FR-SBX-01(Claude 경로)`와 §8-2 잔여를 명시하고 T15 완료로 닫지 않는다.

## 구현 순서와 실패 원칙

1. [H]가 아래 승인 표의 token scope 및 실 세션 위치를 결정한다. CI에는 token을
   추가하지 않는다.
2. pinned Node/Claude image build·digest·크기 probe를 먼저 수행한다. pull/build
   실패는 인프라 실패로 남긴다.
3. 기존 `ProcessEndpoint`에 Claude adapter를 연결하고, `none` 분기를 회귀
   시킨다. process/approval nominal type과 T10 lifecycle을 재사용한다.
4. token env 주입과 redaction/만료 done(error) 단위 테스트를 추가한다.
5. 자격증명 없는 Linux CI gate를 통과시킨 뒤 [H] 실 세션을 별도로 수행한다.
   실 세션이 실패하면 BLOCKED.md에 원인과 증거를 남기고 API key나 장기 token으로
   우회하지 않는다.

타임아웃을 늘려 통과시키기, Claude 실행을 host로 폴백하기, token을 로그에
기록하기, container socket을 mount하기, fixture 밖 native 동작을 지원한다고
주장하기, Linux 조건을 skip하기는 모두 금지한다.

## 승인 요청

| 항목 | 승인 주체 | 승인 범위/판단 |
|---|---|---|
| access token을 spawn env로 주입하는 구현 | [H] | OAuth access token만, refresh token·API key·Keychain mount 없음. 만료는 `done/error`, fallback 없음. |
| **스코프 축소 미충족의 수용 또는 FR-SBX-04 개정** | **명세 소유자 [H]** | access token이 계정 전체 권한이라는 잔여를 **문서화된 부분 충족으로 수용**한다. FR-SBX-04를 하향 개정하지 않으며, 스코프 토큰이 발급되는 날 후속 작업으로 남긴다. |
| pinned Node 22 + Claude 2.1.252 image | [H]/CI 운영 | base digest·npm exact version·최종 image digest/실측 크기 고정. 외부 pull 실패는 인프라 오류. |
| ProcessEndpoint adapter split + existing approval relay | T10/T9 계약 소유자 | `none` host procgroup 보존, `local-podman`만 container Claude, relay 프로토콜 변경 없음. |
| 실 세션 위치와 증거 보존 | [H] | **macOS Podman VM 선택**; 인증/adapter 보조 검증만 수행하고 Linux world 게이트 증거와 결합하지 않는다. token 값은 기록하지 않음. |
| T15 완료 판정 | [H] | CI 무자격증명 기판 게이트와 [H] 실 세션 child-span 증거가 모두 있어야 FR-SBX-01 Claude 행을 닫음. |
