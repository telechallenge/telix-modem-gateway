# Project: Telix Modem Gateway

**Last Updated:** 2026-03-13

## Overview

Virtual Hayes-compatible modem gateway. Telnet clients connect, issue AT commands, and dial phone numbers to reach backend BBS systems via TCP. Includes a web terminal client (Node.js/WebSocket → xterm.js).

## Technology Stack

- **Language:** Go 1.24 (gateway), Node.js (web client)
- **Dependencies:** `golang.org/x/time` (rate limiting), `lumberjack.v2` (log rotation), `yaml.v3` (config)
- **Web client:** Express, `ws` (WebSocket), xterm.js (browser terminal)
- **Container:** Docker multi-stage build, Docker Compose (gateway + fail2ban + web client)
- **CI:** SLSA Go releaser (`.github/workflows/go-ossf-slsa3-publish.yml`)

## Directory Structure

```
cmd/telix/main.go          # Entry point — config load, logger init, server start
internal/
  config/                   # YAML config parsing, phonebook lookup, number normalization
  dialer/                   # Outbound TCP+telnet dialer, TelnetFilter (IAC state machine)
  logging/                  # Custom structured logger (JSON/text), log rotation, sanitization
  modem/                    # AT command parser, modem state machine, S-registers, result codes
  server/                   # TCP listener, session lifecycle, rate limiter, connection tracker
web/
  server.js                 # Express + WebSocket proxy (bridges browser ↔ Telix via TCP)
  public/                   # Static frontend: xterm.js terminal, CP437 font, CSS
configs/telix.yaml          # Default config (phonebook, rate limits, logging)
fail2ban/                   # fail2ban filter + jail config for abuse prevention
```

## Development Commands

| Task | Command |
|------|---------|
| Build | `make build` |
| Run | `make run` (builds first, uses `configs/telix.yaml`) |
| Test | `make test` or `go test ./...` |
| Vet | `make vet` or `go vet ./...` |
| Format | `make fmt` or `go fmt ./...` |
| Docker (full stack) | `make docker-up` (gateway + fail2ban + web) |
| Docker down | `make docker-down` |
| Web client install | `make web-install` |
| Web client dev | `make web-dev` |
| Connect manually | `telnet localhost 2323` |

## Key Architecture

- **Session lifecycle:** `server.go` accepts TCP → creates `Session` → `session.go` runs command/data loop → `modem` package handles AT parsing + state
- **Modem state:** `StateCommand` (reads AT commands) ↔ `StateData` (bridges client ↔ remote BBS)
- **Phonebook matching:** Numbers normalized (strip `-().# ` etc.), entries can require `init`, `baud`, `error_correction`, `compression` — mismatched settings produce garbage + NO CARRIER
- **Telnet filtering:** `TelnetFilter` (IAC state machine) used in both directions — filters commands, passes data, responds to negotiations. Duplicated in Go (`internal/dialer`) and JS (`web/server.js`)
- **Security:** Rate limiter (per-IP + global), connection tracker (max total + per-IP), write deadlines (anti-Slowloris), command length limits, log field sanitization, escape sequence injection protection, network-restricted outbound dials (`dialer.allowed_networks`)
- **Logging:** SessionLogger embeds `session_id` + `source_ip` in every log event for correlation
- **CP437 encoding:** Banner uses raw CP437 bytes; web client translates CP437 → Unicode for browser display
- **Version:** Injected at build time via `-ldflags -X main.version=...` from `git describe`. Used in banner, ATI response.
- **Web client:** Per-IP WebSocket connection limiting (default 5, `MAX_WS_PER_IP` env var)

## Testing

- Tests in `internal/config/`, `internal/modem/`, `internal/dialer/`, `internal/server/`
- Table-driven tests with `t.Run()` subtests
- Run: `go test ./...`

## Important Notes

- `required_init` field is deprecated — auto-migrated to `required_settings.init` at config load
- Error correction (`&Q5`) and compression (`%C1`) default to `true` in both modem state and phonebook entries
- S12 (escape guard time) has a minimum of 20 (400ms) to prevent trivial escape attacks
- The web client's CP437→Unicode table must stay in sync with the Go banner's byte values
- `dialer.allowed_networks` restricts outbound dials to configured CIDRs — **use in production** to prevent the gateway from reaching arbitrary hosts
