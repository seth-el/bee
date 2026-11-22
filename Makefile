BEE_OUTPUT ?= bee

.PHONY: build test eval

build:
	go build -o "$(BEE_OUTPUT)" ./cmd/bee

test:
	go test ./...

eval:
	go run ./cmd/bee-eval run testdata/eval
