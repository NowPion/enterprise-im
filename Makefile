.PHONY: build test test-race run

build:
	cd backend && mkdir -p bin && go build -o bin/enterprise-im-server ./cmd/server

test:
	cd backend && go test -v ./...

test-race:
	cd backend && go test -race -v ./...

run:
	cd backend && go run ./cmd/server
