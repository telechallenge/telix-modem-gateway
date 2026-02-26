BINARY    := telix
CMD       := ./cmd/telix
CONFIG    := configs/telix.yaml
GO        := go
GOFLAGS   := -trimpath
LDFLAGS   := -s -w

.PHONY: all build clean test vet fmt run docker docker-up docker-down

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
