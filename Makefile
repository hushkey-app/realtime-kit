.PHONY: all build run dev dev-config dev-sidecar test vet fmt tidy clean

MODULE := github.com/hushkey-app/realtime-kit
BINARY := realtime-kit
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.builtAt=$(BUILT_AT)

# `run` and `dev` source .env when there is one, so a local checkout needs no
# exports. Everything else about the service reads the environment directly, so
# in production you set the variables the way you set any others — this is a
# development convenience, not a config mechanism.
#
# `set -a` exports every assignment the file makes; sourcing it in the same
# shell as the binary is what puts them in its environment. Note this beats an
# already-exported value, which is the usual .env bargain: the file wins because
# it is the more specific statement of intent for this checkout.
ENV := set -a; [ -f .env ] && . ./.env; set +a;

all: build

build:
	go build -trimpath -ldflags='$(LDFLAGS)' -o $(BINARY) .

run: build
	@$(ENV) ./$(BINARY)

dev:
	@./scripts/dev.sh

dev-config:
	@./scripts/show-dev-config.sh

# Start only the HTTP sidecar. Most local development wants `make dev`, which
# also starts a native LiveKit media server with a matching generated key pair.
dev-sidecar:
	@$(ENV) go run .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
