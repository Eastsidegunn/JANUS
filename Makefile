GO ?= go

.PHONY: lint test fixtures codegen codegen-drift ci

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
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt 필요:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) run ./tools/boundarylint

test:
	$(GO) test -race ./...

# T8/T9에서 어댑터 골든 픽스처가 생기면 실제 대조로 대체된다.
fixtures:
	@echo "fixtures: 등록된 어댑터 픽스처 없음 (T8/T9에서 활성화)"

ci: lint test fixtures codegen-drift
