GO ?= go

.PHONY: lint test fixtures ci

lint:
	$(GO) vet ./...
	$(GO) run ./tools/boundarylint

test:
	$(GO) test ./...

# T8/T9에서 어댑터 골든 픽스처가 생기면 실제 대조로 대체된다.
fixtures:
	@echo "fixtures: 등록된 어댑터 픽스처 없음 (T8/T9에서 활성화)"

ci: lint test fixtures
