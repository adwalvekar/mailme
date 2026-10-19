.PHONY: build test check

build:
	go build -o bin/mailme ./cmd/mailme

test:
	go test -race ./...

check: test
	go vet ./...
