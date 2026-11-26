# Collie — build a single self-contained binary (web UI embedded).
BIN      ?= bin/collie
GOFLAGS  ?= -trimpath
LDFLAGS  ?= -s -w

.PHONY: all build web go dev clean install cross

all: build

build: web go

web: web/node_modules
	cd web && npm run build
	@touch web/dist/.keep

web/node_modules: web/package.json
	cd web && npm install --no-audit --no-fund
	@touch web/node_modules

go:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) .

# Cross-compile for a server (e.g. `make cross GOARCH=arm64`).
cross: web
	CGO_ENABLED=0 GOOS=linux GOARCH=$(or $(GOARCH),amd64) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/collie-linux-$(or $(GOARCH),amd64) .

# Backend on :7878 + Vite dev server with hot reload on :5173.
dev:
	@echo "run in two terminals:  go run . --no-auth   |   cd web && npm run dev"

install: build
	./deploy/install.sh

clean:
	rm -rf bin web/dist/assets web/dist/index.html
