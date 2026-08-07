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

.PHONY: monitoring-up monitoring-down monitoring-logs
# `docker compose up -d` already starts these; these targets are for working on
# the monitoring stack without cycling the gateway.
MONITORING_SVCS := prometheus node-exporter grafana bbs-exporter fail2ban-exporter \
                   docker-exporter docker-socket-proxy

monitoring-up:
	docker compose up -d $(MONITORING_SVCS)

monitoring-down:
	docker compose stop $(MONITORING_SVCS)

monitoring-logs:
	docker compose logs -f $(MONITORING_SVCS)

.PHONY: monitoring-reload
# Prometheus' config is a single-file bind mount, so an editor that writes a new
# file (rather than truncating in place) leaves the container pointing at the old
# inode -- `/-/reload` then re-reads the *old* config and the change silently does
# nothing. Recreating the container is the only reliable way to pick up an edit.
monitoring-reload:
	docker compose up -d --force-recreate prometheus

.PHONY: bans unban unban-all
# Two jails ban independently — `telix` for 2h, `recidive` for a week when an
# IP has been banned repeatedly — so unbanning one jail is not enough to let
# someone back in. `fail2ban-client unban <IP>` clears every jail at once, which
# is why these targets never name a jail.
bans:
	@for jail in telix recidive; do \
	  echo "[$$jail]"; \
	  docker compose exec -T fail2ban fail2ban-client status $$jail \
	    | grep -E 'Currently banned|Banned IP list' \
	    | sed 's/^[ |`-]*/  /'; \
	done

unban:
	@test -n "$(IP)" || { echo "usage: make unban IP=203.0.113.9"; exit 1; }
	docker compose exec -T fail2ban fail2ban-client unban $(IP)

unban-all:
	docker compose exec -T fail2ban fail2ban-client unban --all

web-install:
	cd web && npm install

web-dev:
	cd web && npm run dev

.PHONY: web-test
web-test:
	cd web && npm test

.PHONY: web-e2e
web-e2e:
	cd web && npm run e2e

.PHONY: exporter-test
# Stdlib unittest — none of the exporters have dependencies to install.
exporter-test:
	@for d in fail2ban-exporter bbs-exporter docker-exporter vbox-exporter; do \
	  echo "== $$d"; \
	  ( cd monitoring/$$d && python3 -m unittest discover -p 'test_*.py' -q ) || exit 1; \
	done

.PHONY: dashboards
# The dashboard JSON is generated; edit generate.py, run this, commit both.
dashboards:
	python3 monitoring/grafana/dashboards/generate.py

.PHONY: vbox-exporter-install vbox-exporter-status vbox-exporter-logs
# The VirtualBox exporter is the one piece that cannot live in a container:
# VBoxManage talks to VBoxSVC over the session owner's XPCOM IPC socket. It runs
# as a systemd --user unit, so none of this needs sudo. Lingering is what keeps
# it up when nobody is logged in.
vbox-exporter-install:
	mkdir -p $(HOME)/.config/systemd/user
	ln -sf $(CURDIR)/monitoring/vbox-exporter/telix-vbox-exporter.service \
	       $(HOME)/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now telix-vbox-exporter
	@loginctl show-user "$(USER)" -p Linger | grep -q Linger=yes \
	  || echo "NOTE: run 'loginctl enable-linger $(USER)' so it survives logout"
	@systemctl --user is-active telix-vbox-exporter

vbox-exporter-status:
	@systemctl --user status telix-vbox-exporter --no-pager || true

vbox-exporter-logs:
	journalctl --user -u telix-vbox-exporter -f
