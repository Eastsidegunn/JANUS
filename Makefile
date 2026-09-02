GO ?= go

.PHONY: lint test smoke fixtures codegen codegen-drift world-integration extensions-integration ci ci-linux

codegen:
	$(GO) run ./tools/schemagen -out contracts/gen contracts/events.schema.json:EventRecord contracts/wire.schema.json

# 임시 디렉터리에 재생성한 뒤 $(GEN_DIR)과 파일 집합까지 완전 비교한다.
# git 상태 기반 검사는 (a) 미추적 신규 파일, (b) 더 이상 생성되지 않는
# 추적된 stale 파일을 각각 놓칠 수 있어 쓰지 않는다. diff -r은 내용 차이,
# 한쪽에만 있는 파일(신규·stale·삭제) 전부에서 실패한다.
# GEN_DIR 오버라이드는 drift 회귀 테스트가 사본을 훼손할 때 쓴다.
GEN_DIR ?= contracts/gen

codegen-drift:
	@tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(GO) run ./tools/schemagen -out "$$tmp" contracts/events.schema.json:EventRecord contracts/wire.schema.json >/dev/null || exit 1; \
	if ! diff -r "$$tmp" "$(GEN_DIR)"; then \
		echo "codegen drift 검출 — 스키마 재생성 결과와 $(GEN_DIR)이 어긋남 (위 diff 참조)"; \
		exit 1; \
	fi

lint:
	$(GO) vet ./...
	# smoke 하네스는 빌드 태그로 CI에서 격리되지만(실 자격증명 필요),
	# 컴파일은 확인해 코드 부패를 막는다. 실행은 사람 몫이다([H]).
	$(GO) vet -tags smoke ./seams/subagent/claudecode/
	$(GO) mod tidy -diff
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt 필요:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) run ./tools/boundarylint

test:
	$(GO) test -race ./...

# NFR-04/T3 CI 조건: 순수 Go 경로(CGO_ENABLED=0)의 네이티브 smoke 실행.
# -race 테스트와 분리한다 — race는 CGO 경유라 이 경로를 검증하지 못한다.
smoke:
	CGO_ENABLED=0 $(GO) test -count=1 ./core/logd/... ./seams/...

# T2의 expected-fail 속성 테스트는 전부 배선 완료됐다: FR-LOG-06은 T4,
# FR-POL-03은 T6에서 본 스위트(make test)로 편입 — xfail 게이트는 소멸.

# 어댑터 골든 픽스처 대조 (FR-ADP-05, T9에서 활성화).
# 1층: 매니페스트·비밀 게이트 — 픽스처 구성 자체의 무결성
# 2층: 원본 포맷 fingerprint — 대상 도구의 출력 포맷 변경 검출(15건 전체)
# (Claude 8건의 정규화 골든은 seams/subagent/claudecode 테스트가 담당)
fixtures:
	tools/check-fixture-secrets.sh contracts/fixtures
	tools/check-fixture-manifest.sh contracts/fixtures 15
	$(GO) run ./tools/fixtureprint

# T10의 실제 배포 경계 관통 게이트. macOS/Podman 부재를 테스트 없음으로
# 처리하지 않는다: Linux rootless/native-overlay 조건은 테스트 안에서도 다시
# 검사하며 하나라도 없으면 명시적으로 실패한다.
world-integration:
	@if [ "$$($(GO) env GOOS)" != "linux" ]; then \
		echo "world-integration은 Linux 실물 게이트다 — 현재 $$($(GO) env GOOS), skip 금지"; exit 1; \
	fi
	@command -v podman >/dev/null || { echo "world-integration: podman 없음, skip 금지"; exit 1; }
	$(GO) test -tags worldintegration -count=1 -timeout=9m ./surfaces/hx -run '^TestWorldIntegration$$'

# T13 §7: real local artifact HTTP fetch + rootless Podman provisioning gate.
# Linux/rootless/native-overlay/Podman prerequisites are failures, never skips.
extensions-integration:
	@if [ "$$($(GO) env GOOS)" != "linux" ]; then \
		echo "extensions-integration은 Linux 실물 게이트다 — 현재 $$($(GO) env GOOS), skip 금지"; exit 1; \
	fi
	@command -v podman >/dev/null || { echo "extensions-integration: podman 없음, skip 금지"; exit 1; }
	$(GO) test -tags extensionsintegration -count=1 -timeout=9m ./surfaces/hx -run '^TestExtensionsIntegration$$'

ci: lint test smoke fixtures codegen-drift

# GitHub ubuntu runner는 일반 CI와 T10 실물 게이트를 모두 통과해야 한다.
ci-linux: ci world-integration extensions-integration
