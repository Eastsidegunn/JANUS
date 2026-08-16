GO ?= go

.PHONY: lint test smoke test-xfail fixtures codegen codegen-drift ci

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

# T2/T2.1: 구현 전 속성 테스트 — "미배선 sentinel로 인한 실패"만 기대 상태다.
# 테스트별로 정확한 이름으로 각각 실행하고, (1) 통과, (2) panic·timeout·환경
# 오류, (3) sentinel이 아닌 실제 속성 위반은 전부 게이트 실패로 처리한다.
# 예상 밖 실패의 출력은 그대로 표시한다.
# vet이 먼저 도는 이유: 컴파일 오류를 "예상된 실패"로 오인하지 않기 위해.
# FR-LOG-06 리플레이 결정론은 T4에서 배선되어 본 스위트(make test)로 편입됐고,
# 남은 xfail은 FR-POL-03(T6에서 배선) 하나다.
test-xfail:
	$(GO) vet -tags xfail ./core/...
	@out="$$($(GO) test -tags xfail -timeout 120s -run '^TestPropertyProfileMergeOnlyNarrows$$' ./core/... 2>&1)"; rc=$$?; \
	if [ $$rc -eq 0 ]; then \
		echo "xfail 게이트: TestPropertyProfileMergeOnlyNarrows가 통과함 — 구현이 배선됐다면 xfail 태그를 제거해 본 스위트로 옮겨라"; \
		echo "$$out"; exit 1; \
	fi; \
	if ! echo "$$out" | grep -qF "FR-POL-03: 프로파일 병합 구현이 아직 배선되지 않음"; then \
		echo "xfail 게이트: TestPropertyProfileMergeOnlyNarrows가 미배선 sentinel이 아닌 사유로 실패함 (panic/timeout/실제 속성 위반 의심):"; \
		echo "$$out"; exit 1; \
	fi; \
	if echo "$$out" | grep -qE "panic:|test timed out"; then \
		echo "xfail 게이트: TestPropertyProfileMergeOnlyNarrows 출력에 sentinel과 함께 panic/timeout 흔적이 있음:"; \
		echo "$$out"; exit 1; \
	fi; \
	echo "test-xfail: TestPropertyProfileMergeOnlyNarrows 예상대로 미배선 실패 (FR-POL-03, T6에서 배선)"

# T8/T9에서 어댑터 골든 픽스처가 생기면 실제 대조로 대체된다.
fixtures:
	@echo "fixtures: 등록된 어댑터 픽스처 없음 (T8/T9에서 활성화)"

ci: lint test smoke test-xfail fixtures codegen-drift
