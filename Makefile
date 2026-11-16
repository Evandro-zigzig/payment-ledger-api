GO ?= go
IMAGE ?= novabanco-payment-api:local
GO_FILES := $(shell find cmd internal -type f -name '*.go')

.PHONY: fmt fmt-check test vet build ci docker-build

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "$$unformatted"; \
		exit 1; \
	fi

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	mkdir -p bin
	$(GO) build -o bin/payment-api ./cmd/api

ci: fmt-check test vet build

docker-build:
	docker build --tag $(IMAGE) .
