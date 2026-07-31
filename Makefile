GO ?= go
BIN := bin/api

.PHONY: build run fmt vet test tidy check clean

build:
	$(GO) build -o $(BIN) ./cmd/api

run:
	$(GO) run ./cmd/api

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

check: fmt vet test

clean:
	rm -rf $(BIN)
