BINARY    := telix
CMD       := ./cmd/telix
CONFIG    := configs/telix.yaml
GO        := go
GOFLAGS   := -trimpath
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: all build clean test vet fmt run docker docker-up docker-down web-install web-dev

all: build

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) $(CMD)

clean:
	rm -f $(BINARY)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

run: build
	./$(BINARY) -config $(CONFIG)

docker:
	docker build -t $(BINARY) .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

web-install:
	cd web && npm install

web-dev:
	cd web && npm run dev

.PHONY: web-test
web-test:
	cd web && npm test
