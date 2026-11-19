# The pure-Go olm backend requires the goolm build tag; mautrix v0.33
# otherwise compiles the C libolm backend and fails without libolm installed.
TAGS ?= goolm

.PHONY: build run test vet

build:
	go build -tags $(TAGS) -o matrix-whatsapp-mcp ./cmd/server

run:
	go run -tags $(TAGS) ./cmd/server

test:
	go test -tags $(TAGS) ./...

vet:
	go vet -tags $(TAGS) ./...
