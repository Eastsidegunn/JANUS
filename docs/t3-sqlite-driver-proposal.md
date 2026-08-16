# T3 사전 제안 — SQLite 드라이버 선정 (logd 저장 계층)

상태: **승인됨** (2026-08-17 리뷰 — modernc.org/sqlite v1.56.0 선택·의존성 도입
승인). 본 판은 리뷰 수정 4건(busy_timeout↔취소 충돌, 크래시 복구 실증 계획,
BUSY/백프레셔 분리, 실증 문구 정정)을 반영한 것이다. 이 PR은 문서만 담는다 —
의존성 추가·logd 코드는 T3 구현 브랜치에서.
아래 실증 결과는 이 머신(darwin/arm64, go1.26.6)에서 스크래치 모듈(저장소 밖)로
양쪽 드라이버를 실제 구동해 얻은 값이다. 버전·날짜는 2026-08-17 조회 기준.

## 0. 후보와 기본 사실

| | modernc.org/sqlite | github.com/mattn/go-sqlite3 |
|---|---|---|
| 정확한 버전 | **v1.56.0** (2026-08-03) | **v1.14.49** (2026-07-29) |
| 라이선스 | BSD-3-Clause | MIT |
| 내장 SQLite | **3.53.3** (C→Go 트랜스파일) | **3.53.4** (amalgamation, cgo) |
| 구현 방식 | 순수 Go (ccgo 트랜스파일) | cgo 바인딩 |

두 값 모두 실제 `select sqlite_version()` 질의로 확인했다.

## 1. 의존성 규모

- **modernc**: 모듈 그래프 25개. 단, `go version -m`으로 확인한 **바이너리에 실제
  링크되는 모듈은 10개** — modernc.org/{libc, mathutil, memory, sqlite},
  golang.org/x/sys, dustin/go-humanize, google/uuid, mattn/go-isatty,
  ncruces/go-strftime, remyoudompheng/bigfft. 나머지 15개(cc/v4, ccgo/v4,
  x/tools 등)는 트랜스파일 툴체인의 그래프 전용이다.
- **mattn**: 모듈 그래프 **1개**(자기 자신, 의존 0) — 공급망 최소. 단 cgo이므로
  실질 신뢰 대상에 시스템 C 툴체인이 추가된다.

## 2. CGO_ENABLED=0 정적 빌드와 교차 빌드 (실증)

| | modernc | mattn |
|---|---|---|
| CGO=0 빌드 | OK, **네이티브(darwin/arm64)에서 실행·버전 질의 성공** | 빌드는 성공하나 스텁 — 첫 DB 연결에서 "Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a stub" **오류 반환** (실증 프로그램이 그 오류를 panic 처리했던 것; 스텁 자체는 panic하지 않음) |
| CGO=0 교차 (linux/amd64, linux/arm64, darwin/amd64) | 전부 **빌드 성공, 스텁 아님(순수 Go 경로)** — 교차 바이너리의 실행은 이 머신에서 미실증 | 전부 컴파일되지만 전부 **연결 시 오류를 반환하는 스텁** |
| CGO=1 교차 (darwin→linux/amd64) | 해당 없음(cgo 불요) | 실패 (`runtime/cgo`) — 타깃별 C 크로스 툴체인 필요 |

mattn의 CGO=0 스텁은 특히 위험하다: **CI가 빌드 성공을 보고하지만 배포물이
첫 연결에서 오류를 반환한다.** NFR-04(단일 정적 바이너리)와 정면 충돌.
→ 교차 바이너리 실행 실증의 공백은 **T3 CI에 Ubuntu에서의 CGO_ENABLED=0
네이티브 smoke 실행 단계를 추가**해 메운다(-race 테스트와는 별도 단계 —
race는 CGO를 켠 채 돌기 때문).

## 3. go test -race

양쪽 모두 `-race`로 전체 행동 실증 스위트 green (mattn은 cgo 경유). 차이 없음.

## 4. WAL + synchronous=FULL 내구성 (실증)

양쪽 모두: `journal_mode=WAL` 적용 확인, `PRAGMA synchronous` = 2(FULL) 확인,
INSERT → close → reopen 후 행 보존 확인. NFR-02의 fsync 수준 내구성 전제 충족.

## 5. 연결별 PRAGMA 적용 (실증)

두 드라이버 모두 **DSN 방식으로 풀의 모든 커넥션에 PRAGMA가 적용**됨을
4개 커넥션 동시 검사로 확인했다. modernc v1.56.0은 raw `_pragma=` 외에
**검증형 파라미터**(`_journal_mode`, `_synchronous`, `_busy_timeout`,
`_foreign_keys`)를 제공하며 동작을 실증했다
(`journal_mode=wal, synchronous=2, busy_timeout=250, foreign_keys=1` 확인).
검증형을 raw `_pragma`보다 우선 사용한다.

**실증에서 나온 주의점 2건 (modernc)**:
1. `busy_timeout=0`으로 여러 커넥션을 **동시에** 열면 WAL 잠금 경합으로
   `database is locked (261, SQLITE_BUSY_RECOVERY)`가 발생했다.
   → `journal_mode=WAL`은 **초기화 커넥션 하나에서 순차적으로 1회** 설정한다
   (WAL은 DB 파일의 영속 속성이므로 이후 커넥션에 재설정 불요).
2. **busy_timeout은 context 취소와 충돌한다** (리뷰 재현 → 본 실증으로 확인):
   다른 연결이 write lock을 잡은 상태에서 `busy_timeout=5000`이면 50ms
   deadline의 ExecContext가 **5.055초** 뒤에야 반환됐다(SQLite busy handler는
   설정 시간만큼 누적 대기 후에야 BUSY를 반환). `busy_timeout=0`이면 **990µs**
   만에 즉시 `SQLITE_BUSY(5)` 반환. → busy_timeout은 0(또는 극히 짧게) 두고
   **Go 계층에서 context-aware 재시도**를 구현한다. "lock 상태에서 50ms
   deadline이 실제로 짧게 반환된다"는 회귀 테스트를 T3에 포함한다.

## 6. database/sql 풀과 단일 writer 구성 (실증)

writer 전용 `*sql.DB`에 `SetMaxOpenConns(1)`을 걸고 8개 goroutine이 동시
INSERT하는 구성이 양쪽 모두 -race green. FR-LOG-02(단일 writer)는 이 구성
(writer 핸들 1커넥션 + 읽기 전용 핸들 분리)으로 구현한다.

## 7. SQLITE_BUSY / context cancellation (실증)

- BUSY: 한 핸들이 쓰기 트랜잭션을 잡은 상태에서 두 번째 핸들의 쓰기 →
  modernc `database is locked (5) (SQLITE_BUSY)`, mattn `database is locked`.
  양쪽 모두 오류가 표면화된다.
  **주의 — BUSY는 백프레셔가 아니다**: SQLITE_BUSY는 저장소 잠금 경합이며
  Go 계층 재시도의 대상이고, FR-LOG-09 백프레셔는 **writer의 bounded queue
  포화**에서 발생하는 별개 상태다. 두 상태는 별도 타입·별도 테스트로 다룬다
  (§13 구현 계획 참조).
- context: 50ms deadline의 무거운 재귀 CTE → 양쪽 모두
  `context deadline exceeded`로 취소 전파 확인.

## 8. UPDATE/DELETE 차단 트리거 (실증)

`BEFORE UPDATE/DELETE … RAISE(ABORT, …)` 트리거가 양쪽 모두 동작:
UPDATE/DELETE는 오류(modernc: `constraint failed … (1811)`), INSERT는 통과,
행 수 불변. FR-LOG-01의 저장소 수준 물리 차단 전제 충족.

## 9. close/reopen 복구 (실증의 한계와 T3 계획)

양쪽 모두 close 후 reopen에서 데이터 보존을 확인했다. 단, **정상 close 경로는
SQLite가 마지막 checkpoint까지 수행할 수 있으므로 NFR-02의 "크래시 후 마지막
커밋 복구"의 실증이 아니다.** T3 완료 기준 테스트는 다음 형태로 한다:
helper 프로세스가 커밋 acknowledge 직후 **Close 없이 강제 종료(SIGKILL)**되고,
부모 프로세스가 같은 DB 파일을 다시 열어 **마지막 acknowledge된 seq까지**
복원되는지 확인한다. WAL + synchronous=FULL이 커밋마다 WAL을 동기화한다는
설정 선택 자체는 SQLite 문서와 정합.

## 10. 보안 패치·릴리스 주기

- modernc: v1.54.0(07-15) → v1.55.0(07-20) → v1.56.0(08-03) — 3주에 3릴리스,
  upstream SQLite를 근접 추종. GitLab(cznic/sqlite) 단일 메인테이너 조직.
- mattn: v1.14.48(2026-07-13), v1.14.49(2026-07-29)로 최근 활발하나, GitHub
  릴리스 이력에 v1.14.16(2022-10)→v1.14.48 사이 공백이 있고(태그만 존재)
  open issue 159개. SQLite 보안 패치 반영은 amalgamation 갱신 시점에 종속.

## 11. 빌드 시간·바이너리 크기 (실증, -a 콜드 빌드)

| | modernc | mattn(CGO=1) |
|---|---|---|
| 콜드 빌드 | **3.8s** | 11.0s (sqlite3.c 컴파일) |
| 바이너리 | 9.66MB | 6.87MB |

## 12. NFR-04 정합성

NFR-04: 컨트롤 플레인·collector는 단일 정적 바이너리로 배포 가능(SHOULD).
- modernc: CGO=0 정적 빌드·3플랫폼 교차 빌드가 실증됨 — 정합.
- mattn: cgo 필수 → 정적 빌드는 musl 등 별도 체계 필요, 교차 빌드는 타깃별
  C 툴체인 필요, CGO=0은 침묵 스텁 — 비정합.

---

## 13. 추천안

**modernc.org/sqlite v1.56.0** (BSD-3-Clause).

결정 근거: 두 드라이버는 **행동 실증 전 항목(WAL/FULL, 트리거, BUSY, 취소,
복구, race)에서 동등**했다. 갈린 곳은 빌드·배포 축이며, 여기서 mattn의 cgo
요구는 NFR-04와 정면 충돌하고 CGO=0 침묵 스텁은 CI green ≠ 동작 바이너리라는
함정을 만든다. 이식성·교차 빌드·race 단순성은 이 저장소의 강제 장치 철학과
일치한다.

### 탈락 사유 — mattn/go-sqlite3 v1.14.49
- cgo 필수: NFR-04(정적 바이너리) 비정합, 교차 빌드에 타깃별 C 툴체인 필요.
- CGO_ENABLED=0에서 빌드는 성공하지만 런타임 panic하는 스텁 — 조용한 배포 사고 경로.
- SQLite 최신 추종(3.53.4)과 최소 공급망(의존 0)은 인정되는 장점이나,
  위 배포 축 결함을 상쇄하지 못한다.

### 공급망 변화 (승인 대상)

| 구분 | 내용 |
|---|---|
| 직접 의존 추가 | `modernc.org/sqlite` **v1.56.0** (BSD-3-Clause) |
| 바이너리 링크 간접 의존 | modernc.org/{libc v1.74.4, mathutil v1.7.1, memory v1.11.0}, golang.org/x/sys v0.47.0, dustin/go-humanize, google/uuid, mattn/go-isatty, ncruces/go-strftime, remyoudompheng/bigfft (9개) |
| 그래프 전용(링크 안 됨) | modernc.org/cc/v4, ccgo/v4, gc/v2·v3, x/tools 등 15개 — go.sum에는 편입 |
| import 지점 | logd의 store 구현(예: `seams/store/sqlite`) 한 곳으로 한정 |

### 예상 DSN/PRAGMA와 구조 (T3 구현 계획, 2026-08-17 리뷰 반영)

- **초기화**: 전용 커넥션 1개에서 순차 1회 — `PRAGMA journal_mode=WAL`,
  스키마 생성, `RAISE(ABORT)` 트리거 2종(§8 실증 형태) 설치.
- **풀 DSN** (검증형 파라미터 우선):
  `file:<session>.db?_synchronous=FULL&_busy_timeout=0&_foreign_keys=1`
  — busy_timeout=0 + **Go 계층 context-aware 재시도**(§5 실증 2번 근거).
- writer: 전용 `*sql.DB`, `SetMaxOpenConns(1)` — FR-LOG-02 단일 writer.
- reader: 별도 핸들(`?mode=ro` 검토), writer와 분리.
- **소유권**: `core/logd`가 writer와 store **인터페이스**를 소유하고
  SQLite 구현은 `seams/store/sqlite`에 둔다(§3.1). T3 범위에
  FR-LOG-07(raw 보존)·FR-LOG-08(저장 전 redaction 패스) 포함.
- **BUSY ≠ 백프레셔 분리**: SQLITE_BUSY는 store 계층의 재시도 대상 오류 타입,
  FR-LOG-09 백프레셔는 writer bounded queue 포화의 별도 타입. 테스트:
  포화 시 입력 중단 → 공간 확보 후 재개 → 수락된 이벤트 전량 최종 저장 →
  유실 0.
- **회귀 테스트**: lock 상태에서 50ms deadline이 짧게 반환(§5 실증 2번),
  crash 복구(§9 helper 프로세스 SIGKILL).
- **경계 강제**: `modernc.org/sqlite` import를 `seams/store/sqlite`로만
  한정하는 규칙을 **boundarylint에 추가**한다 — 문서 선언이 아니라 린트로 강제.
- **CI**: Ubuntu에서 CGO_ENABLED=0 네이티브 smoke 실행 단계 추가
  (-race 테스트와 분리 — race는 CGO 경유).

### 리스크 기록
- modernc는 트랜스파일 구현이라 극단 경로에서 upstream C와 미세한 차이 가능성이
  있다 — logd의 내구성·트리거·복구 테스트(T3 완료 기준)가 해당 경로를 직접
  검증하므로 회귀는 CI에서 잡힌다. SQLite 버전이 upstream보다 한 스텝 늦을 수
  있다(현재 3.53.3 vs 3.53.4).
- 공급망이 mattn 대비 크다(링크 10 모듈). go.sum 고정 + import 지점 한정으로 관리.

승인되면 T3 구현 브랜치에서 의존성 도입과 logd 구현을 시작한다.
