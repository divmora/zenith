.PHONY: build clean test test-coverage fmt lint run

BINARY  ?= zenith
VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X github.com/divmora/zenith/internal/version.Version=$(VERSION)

build:
	@go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/zenith

clean:
	@rm -rf bin/ dist/ coverage.out coverage.html

test:
	@go test -race ./...

test-coverage:
	@go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -html=coverage.out -o coverage.html

fmt:
	@gofmt -w .

lint:
	@golangci-lint run ./...

run: build
	@./bin/$(BINARY) $(ARGS)
