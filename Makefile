GO ?= go

.PHONY: lint test fixtures codegen codegen-drift ci

codegen:
	$(GO) run ./tools/schemagen -out contracts/gen contracts/events.schema.json:EventRecord contracts/wire.schema.json

# 재생성 후 수정·삭제·미추적 신규 파일이 하나라도 있으면 drift다.
# git diff --exit-code는 미추적 파일을 놓치므로 쓰지 않는다.
codegen-drift: codegen
	@status="$$(git status --porcelain --untracked-files=all -- contracts/gen)"; \
	if [ -n "$$status" ]; then \
		echo "codegen drift 검출 — 스키마와 커밋된 생성물이 어긋남:"; \
		echo "$$status"; \
		git --no-pager diff -- contracts/gen; \
		exit 1; \
	fi

lint:
	$(GO) vet ./...
	$(GO) run ./tools/boundarylint

test:
	$(GO) test -race ./...

# T8/T9에서 어댑터 골든 픽스처가 생기면 실제 대조로 대체된다.
fixtures:
	@echo "fixtures: 등록된 어댑터 픽스처 없음 (T8/T9에서 활성화)"

ci: lint test fixtures codegen-drift
