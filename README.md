# Telix Modem Gateway

A virtual Hayes-compatible modem gateway that sits between telnet clients and BBS systems. Clients connect via telnet and interact with a realistic AT command simulation — dialing phone numbers, hearing rings, negotiating connections — before being bridged to a real TCP host. Think of it as a modem simulator that turns any telnet connection into an authentic 1990s dial-up experience.

## Use Cases

Providing a gateway to provide a "modem" style experience connecting to telnet BBS'.  We also act as a proxy/gateway to hide the remote BBS' IP or even allow BBS to exist in a private network accessible to the gateway, for instance inside kubes.

## How it works

1. A user connects to the gateway via telnet (default port 2323)
2. They see a banner and an `OK` prompt, just like a real modem
3. They issue AT commands to configure the modem and dial a number (e.g. `ATDT916-555-1212`)
4. The gateway looks up the number in a phonebook, rings, and bridges the connection to the configured TCP host
5. The user interacts with the remote BBS as if they'd dialed in on a real modem
6. `+++` (with guard time pauses) escapes to command mode; `ATH` hangs up

Phonebook entries can require specific modem settings before they'll connect cleanly — wrong settings produce garbage output and `NO CARRIER`, just like a real misconfigured modem.

## Building

Requires Go 1.21+.

```sh
# Build the binary
make build

# Or directly
go build -trimpath -ldflags '-s -w' -o telix ./cmd/telix
```

## Running

```sh
# Run with default config
./telix -config configs/telix.yaml

# Show version
./telix -version

# Or use make
make run
```

The gateway listens for telnet connections on the configured port (default 2323). Connect with any telnet client:

```sh
telnet localhost 2323
```

### Docker

```sh
# Build and run with Docker Compose
# (gateway, fail2ban, web client, nginx, Prometheus, node_exporter, Grafana,
#  BBS + Docker exporters)
make docker-up

# Or just the container
make docker
docker run -p 2323:2323 -v ./configs/telix.yaml:/etc/telix/telix.yaml:ro telix
```

## Banner art

Drop ANSI art into `banners/` as `.ANS` files and the gateway greets each caller
with one of them at random, in place of the built-in TELIX logo. Compose mounts
the directory read-only at the path `server.banner_dir` points to:

```yaml
# configs/telix.yaml
server:
  banner_dir: /etc/telix/banners   # ./banners on the host; unset to disable
```

```sh
cp my-art.ans banners/            # takes effect on the next call, no restart
```

The directory is re-read per connection, so art can be added or removed under a
running gateway. An empty directory — or no `banner_dir` at all — falls back to
the built-in banner, so this is entirely optional.

Details worth knowing:

- Files are matched by the `.ans` extension, any case. Dotfiles, subdirectories
  and anything else (including `banners/README.md`) are ignored.
- The SAUCE metadata trailer the DOS art tools append is stripped, along with
  its comment block, so it doesn't print as a line of mojibake under the art.
- DOS `CRLF` art is sent as-is; a bare `LF` gets its carriage return back so the
  art doesn't staircase down the screen.
- Art is authored for **80 columns**, which is what the web terminal is pinned
  to. A piece filling all 24 rows is fine: the cursor is parked clear of the
  bottom so the `OK` that follows doesn't scroll the top off.
- Files over 512 KB are skipped.
- The piece is chosen once per session, so `ATCLS` redraws the same one.

Running outside Docker (`make run`)? Point `banner_dir` at `./banners`.

## Bans

fail2ban watches the gateway's log and blocks abusive IPs. Two jails ban
independently, which is the thing to know before you try to let someone back in:

| Jail | Trigger | Ban |
|---|---|---|
| `telix` | 10 rate-limit / invalid-command / rejected-connection events in 15 min | 15 minutes |
| `recidive` | banned 50 times in a day | 7 days |

A short `telix` ban makes `recidive` *easier* to reach, not harder — an IP that
cycles back in sooner can collect its bans faster. At a 15-minute bantime an IP
can be banned up to 96 times a day, which is why `recidive` needs 50 rather than
the stock 3 to mean "genuinely hostile" instead of "persistent".

An IP can sit in both, so clearing one jail does not let it back in.
`fail2ban-client unban` clears every jail at once, which is what these wrap:

```sh
make bans                      # who is banned, per jail
make unban IP=203.0.113.9      # release one IP, from every jail
make unban-all                 # release everyone
```

Bans also show up on the Grafana dashboard under **Abuse** — a count per jail, a
table of who is blocked and for how long, and the ban population over time. The
per-IP series are capped at 50 per scrape, because an IP that reaches the
exporter is attacker-controlled and an unbounded label set is how you take
Prometheus down; whatever the cap drops is published as "not listed (cap)"
rather than silently vanishing. The counts are always exact.

> **Note:** fail2ban's chain hangs off `INPUT`, so bans bite callers who telnet
> straight to the published port. Traffic arriving through the web terminal
> crosses `FORWARD` instead and is not affected — and it all arrives from the
> web proxy's container IP anyway, so a ban there would hit every browser user
> at once. The gateway's own per-IP rate limiter is what covers that path.

## Monitoring

`docker compose up -d` brings up Prometheus, node_exporter and Grafana alongside
the gateway. Grafana auto-provisions the Prometheus datasource and seven
dashboards. There is nothing to import.

| Dashboard | Covers |
|-----------|--------|
| **Telix Modem Gateway** | The gateway, the web terminal, bans, host vitals |
| **BBS — Fungus Land** / **Happy Friends BBS** / **HPACV Heaven** | The three ENiGMA½ boards in `~/bbs/bbs_infra`: callers, users, messages, files, uploads, plus container resources |
| **BBS — Boringland Filedrop** / **Warez Server** | Container resources only — see below |
| **VirtualBox VMs** | Every registered VM, measured from the host |

```sh
cp .env.example .env      # set MONITORING_BIND_TAILSCALE and the Grafana password
make docker-up
make vbox-exporter-install   # only if you want the VirtualBox dashboard
# Grafana    http://127.0.0.1:3001   (admin / whatever you set in .env)
# Prometheus http://127.0.0.1:9090
```

### Exposure

Grafana and Prometheus publish **only** to the two addresses in `.env`:

| Variable | Purpose |
|----------|---------|
| `MONITORING_BIND_LOCAL` | Loopback. Leave as `127.0.0.1`. |
| `MONITORING_BIND_TAILSCALE` | This host's Tailscale address (`tailscale ip -4`). Set to `127.0.0.1` if you don't use Tailscale. |

Do not set either to `0.0.0.0` or leave them empty — that publishes Grafana and
an unauthenticated Prometheus to the internet. Every other target — node_exporter,
the BBS and Docker exporters, and both gateway `/metrics` endpoints — is
reachable only from inside the compose network and is never published to the
host.

The one exception is the VirtualBox exporter, which has to run on the host (see
below). It binds the Docker bridge address `172.17.0.1`, not `0.0.0.0`: every
container can reach it, nothing off the host can. Pass `--bind` if your bridge
sits elsewhere — `ip -4 addr show docker0` will tell you.

Grafana persists its admin password on first start, so changing
`GRAFANA_ADMIN_PASSWORD` afterwards has no effect — change it in the UI, or
remove the `grafana-data` volume to re-seed.

### What's collected

The gateway serves Prometheus metrics on its own listener (port 9101, separate
from the telnet port so a modem session can never reach it), plus `/healthz`.
Configure under `metrics:` in `telix.yaml`; set `enabled: false` to turn the
listener off entirely.

| Metric | Type | Notes |
|--------|------|-------|
| `telix_sessions_active` / `_total` | gauge / counter | Connected clients |
| `telix_session_duration_seconds` | histogram | Buckets span 1s–2h |
| `telix_dials_total` | counter | Labelled `entry` (phonebook **name**) and `outcome` |
| `telix_dial_duration_seconds` | histogram | ATDT to CONNECT or failure |
| `telix_dials_in_flight` | gauge | Dials currently being placed |
| `telix_connections_rejected_total` | counter | `reason`: `rate_limited`, `limit_exceeded` |
| `telix_bytes_total` | counter | `direction`: `to_remote`, `to_client`. Data mode only |
| `telix_build_info` | gauge | Version in the label |
| `go_*` / `process_*` | — | Go runtime and process collectors |

`outcome` is one of `success`, `no_carrier`, `busy`, `timeout`,
`unknown_number`, `auth_failed`, `settings_mismatch`, `blocked`. Dialled digits
are **never** used as a label — an unrecognised number is recorded as
`entry="unknown"`, so a caller sweeping numbers cannot mint one time series per
guess.

The web client exposes `telixweb_*` on its existing port at `/metrics`:
connections active/total, per-IP rejections, proxy errors, and bytes proxied
each direction.

### The BBS boards

`monitoring/bbs-exporter` reads each ENiGMA½ instance's SQLite databases
read-only off a bind mount — the boards sit on isolated Docker networks and
expose no metrics endpoint, so the shared data directory is the only seam.
Instances are **discovered**, not configured: any subdirectory of
`$BBS_INSTANCES_DIR` holding a `db/` is picked up, so a fourth board needs no
change here. Point `BBS_INSTANCES_DIR` in `.env` at your `instances/` directory
if it isn't `~/bbs/bbs_infra/instances`.

| Metric | Type | Notes |
|--------|------|-------|
| `enigma_up` | gauge | Databases readable. **Not** a telnet reachability check |
| `enigma_users` / `_by_status` | gauge | `status`: 0 disabled, 1 inactive, 2 active, 3 locked |
| `enigma_logins_total` | counter | Calls since install |
| `enigma_logins_last_24h` / `enigma_callers_last_24h` | gauge | Calls, and distinct callers |
| `enigma_messages` / `enigma_files` (+ `_by_area`) | gauge | Per-area series capped at 64; the shortfall is published as `enigma_areas_truncated` |
| `enigma_file_bytes` / `enigma_file_downloads_total` | gauge / counter | Held now vs downloads across held files |
| `enigma_uploads_total` / `enigma_upload_bytes_total` | counter | Lifetime |
| `enigma_user_events_recorded` | **gauge** | `event`: `login`, `logoff`, `new_user`, `ul_files`, … |
| `enigma_last_login_/_message_/_upload_timestamp_seconds` | gauge | Absent, not zero, on a board nobody has used |

Two things about these are load-bearing rather than stylistic. ENiGMA trims
`system_event_log` and `user_event_log` with `DELETE`, so anything counting rows
in them is a **gauge** — published as a counter, every trim would read to
Prometheus as a counter reset and `rate()` would invent activity that never
happened. And the databases are WAL-mode: opening one with SQLite's
`immutable=1` silently returns *stale* data (measured on a live board as 12
users where the truth was 15), so the exporter uses `mode=ro` only and has no
fallback — reporting the board down beats reporting a confident wrong number.

**Boringland Filedrop and Warez Server** are not ENiGMA. They share no schema
and expose no metrics, so their dashboards are container-level only: up/down,
CPU, memory, network, restarts. Callers and files are not observable from
outside them.

### Containers

`monitoring/docker-exporter` publishes `docker_container_*` — running, health,
restarts, CPU seconds, memory working set, network bytes, pids.

This replaces cAdvisor, which cannot work here: Docker 29 uses the containerd
image store, so `/var/lib/docker/image` has no `<driver>/layerdb` tree and
cAdvisor's cgroup-to-container mapping fails for every container. It comes up
*healthy* and reports exactly one series — the root cgroup — so nothing ever
appears in a panel.

The exporter never touches the Docker socket. `docker-socket-proxy` holds it and
allows only `GET /containers/*`; the two talk over an `internal: true` network
with no route off the host. Mounting `docker.sock` into a consumer directly
would be equivalent to giving it root, and mounting it `:ro` does not change
that — the socket is an API, and the API can create privileged containers.

Memory limits come from `HostConfig.Memory`, not from the stats API: for an
unlimited container the latter reports the *host's* total RAM, and a "limit"
line at 16 GiB flattens a 10 MiB working set off the axis. Unlimited containers
get no limit series at all.

### VirtualBox VMs

`monitoring/vbox-exporter` is the one piece that cannot live in a container:
VBoxManage talks to VBoxSVC over the session owner's XPCOM IPC socket. It runs
as a **systemd `--user` unit**, so installing it needs no sudo:

```sh
make vbox-exporter-install    # symlinks the unit, enables and starts it
make vbox-exporter-status
```

Prometheus reaches it at `host.docker.internal:9103` via the `extra_hosts` entry
on the prometheus service. The exporter binds to the Docker bridge address
(`172.17.0.1`) rather than `0.0.0.0`, so it is reachable from every container
but not from the LAN. A **down** target here means the unit is not running, not
that the VMs are gone.

Everything is measured from outside the guest — `VBoxManage` plus the
`VBoxHeadless` process in `/proc`. VirtualBox's `Guest/*` metric family is
deliberately not exported: it needs Guest Additions, reads empty on a DOS guest,
and would be guest-reported rather than host-observed.

| Metric | Type | Notes |
|--------|------|-------|
| `vbox_vm_running` / `vbox_vm_state` | gauge | A registered-but-stopped VM reports 0 rather than vanishing |
| `vbox_vm_cpu_seconds_total` | counter | From `/proc`, so Prometheus rates it itself |
| `vbox_vm_cpu_load_ratio` | gauge | VirtualBox's own reading, `mode`: `user`, `kernel` |
| `vbox_vm_memory_resident_bytes` / `vbox_vm_disk_bytes` | gauge | Binary units, confirmed against `ps` and the VDI byte sizes |
| `vbox_vm_configured_memory_bytes` / `_vcpus` | gauge | Reported whether or not the VM is up |
| `vbox_vm_start_time_seconds` / `_threads` | gauge | From `/proc` |
| `vbox_vms_registered` / `vbox_vms_running` | gauge | Fleet totals |

Per-VM disk IO is not exported: Ubuntu's `ptrace_scope=1` denies `/proc/<pid>/io`
even to the uid that owns the process, so it would be permanently empty rather
than merely unavailable.

### Editing the monitoring config

The dashboard JSON is **generated**. Edit
`monitoring/grafana/dashboards/generate.py`, run `make dashboards`, and commit
both — five of the seven dashboards are the same two templates pointed at
different instances, and hand-maintaining them guarantees drift.
(`telix.json` predates the generator and stays hand-written.)

`prometheus.yml` is a single-file bind mount. An editor that writes a *new* file
rather than truncating in place leaves the container on the old inode, and
`/-/reload` then quietly re-reads the old config. Use `make monitoring-reload`,
which recreates the container.

## Configuration

Configuration is a YAML file. See `configs/telix.yaml` for a complete example.

### Server

```yaml
server:
  port: 2323              # Listen port
  max_connections: 100     # Total concurrent connections
  max_per_ip: 5           # Max connections per IP
  idle_timeout: 300        # Seconds before idle disconnect
```

### Logging

```yaml
logging:
  level: info              # debug, info, warn, error
  format: json             # json or text
  file: ./log/telix.log   # Optional log file (also logs to stdout)
```

### Dialer (network restrictions)

```yaml
dialer:
  allowed_networks:    # Restrict outbound dials to these CIDRs
    - "10.0.0.0/8"     # Private network where BBS hosts live
    - "172.16.0.0/12"
    - "192.168.0.0/16"
    - "127.0.0.0/8"    # Loopback
```

When `allowed_networks` is set, the gateway resolves phonebook hostnames and verifies all IPs fall within the allowed CIDRs before connecting. This prevents the gateway from being used to reach arbitrary hosts on the public internet. **Recommended for production deployments** where BBS hosts live on a private network.

If `allowed_networks` is empty or omitted, all destinations are allowed.

### Rate limiting

```yaml
rate_limit:
  enabled: true
  max_attempts: 5          # Max connection attempts per window
  window_seconds: 60       # Rolling window
  block_duration: 300      # Block duration in seconds
```

### Phonebook

The phonebook maps dial strings to TCP hosts. Each entry can optionally require the modem to be configured a certain way before it will connect cleanly.

```yaml
phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 8888
    name: "Enigma Demo"
    required_settings:
      init: "ATZ"          # Modem must have sent this command
      baud: 9600           # Modem must be locked to this speed (via AT&N)

  - number: "415-555-0100"
    host: "bbs.example.com"
    port: 23
    name: "Example BBS"
```

**Phone numbers** are normalized — dashes, dots, spaces, and parentheses are stripped for matching, so `916-555-1212`, `(916) 555-1212`, and `9165551212` all match the same entry.

**Hosts are resolved from inside the gateway container.** Under Docker Compose, `localhost` and `127.0.0.1` refer to the gateway container itself — not your machine. To reach a BBS that publishes a port on the host (including one running in another container), use `host.docker.internal`, which the Compose file maps to the host gateway:

```yaml
  - number: "916-548-3208"
    host: "host.docker.internal"   # not "localhost"
    port: 8403
```

If `allowed_networks` is set, make sure it covers the Docker bridge (`172.16.0.0/12`) — `127.0.0.0/8` will not match `host.docker.internal`.

#### Required settings

Both fields are optional. If omitted, the entry connects with no preconditions.

| Field | Description |
|-------|-------------|
| `init` | An AT command string (e.g. `ATZ`, `AT&F`) the user must have sent before dialing. If not sent, the connection produces garbage and drops. |
| `baud` | A baud rate the modem must be locked to via `AT&N`. If the modem's speed doesn't match, the connection produces garbage and drops. Valid rates: 300, 1200, 2400, 4800, 7200, 9600, 14400, 19200, 38400, 56000. |
| `error_correction` | Whether V.42/LAPM error correction must be on (`true`) or off (`false`). **Defaults to `true`** if not specified — matching the modem's factory default (`&Q5`). Set to `false` for entries that require a raw connection. |
| `compression` | Whether V.42bis compression must be on (`true`) or off (`false`). **Defaults to `true`** if not specified — matching the modem's factory default (`%C1`). Set to `false` for entries that require no compression. |

When `baud` is specified, a successful connection reports `CONNECT <baud>` at that speed. When not specified, the modem uses its locked speed (if set) or picks a realistic random speed from a weighted pool.

**Backward compatibility**: The older `required_init` field still works and is automatically migrated to `required_settings.init` at load time.

#### Password-gated entries

An entry with a `password` field asks for it before bridging the call:

```yaml
  - number: "555-0199"
    host: "private.example.com"
    port: 23
    name: "Private BBS"
    password: "swordfish"
```

The modem rings and reports `CONNECT` first, then a `PASSWORD:` prompt appears — so it reads as the BBS's own login rather than something the gateway printed. Input is shadow-echoed as `*` regardless of `ATE0`/`ATE1`. One attempt only: a wrong password drops the call with `NO CARRIER`, and the outbound connection is never opened, so a failed guess never reaches the remote host.

#### Always-busy entries

An entry with `busy: true` is a line that is permanently engaged — dialing it always reports `BUSY`:

```yaml
  - number: "555-0142"
    name: "Single-line BBS"
    busy: true
```

The caller hears no ringing (the telco returns busy tone instead of ringback), and **no outbound connection is ever placed** — which makes this the way to take a BBS out of service without removing its number from the phonebook. `host` and `port` are unused and may be omitted; `required_settings` and `password` are never reached, since a busy line is engaged before any negotiation or login. Dials are counted in `telix_dials_total` with `outcome="busy"`.

## AT command reference

### Standard commands

| Command | Description |
|---------|-------------|
| `AT` | Attention — returns `OK` |
| `ATZ` | Reset modem to defaults |
| `AT&F` | Factory reset (clears init history) |
| `ATDT<number>` | Dial number (tone) |
| `ATDP<number>` | Dial number (pulse) |
| `ATH` / `ATH0` | Hang up |
| `ATH1` | Go off-hook |
| `ATO` | Return to data mode (after `+++` escape) |
| `ATE0` / `ATE1` | Echo off / on |
| `ATV0` / `ATV1` | Verbose off / on (numeric vs word result codes) |
| `ATQ0` / `ATQ1` | Quiet off / on (suppress result codes) |
| `ATX0`–`ATX4` | Result code level (X0 = basic, X4 = full with dial tone/busy detection) |
| `ATI` | Identify modem |
| `AT&V` | View active configuration |
| `AT&Q0` / `AT&Q5` | Error correction off / on (V.42/LAPM) |
| `AT%C0` / `AT%C1` | Data compression off / on (V.42bis) |

### S-registers

| Command | Description |
|---------|-------------|
| `ATS<n>?` | Query register value |
| `ATS<n>=<v>` | Set register value (0–255) |

Key registers:

| Register | Default | Description |
|----------|---------|-------------|
| S0 | 0 | Auto-answer ring count (0 = disabled) |
| S2 | 43 | Escape character (ASCII `+`) |
| S3 | 13 | Carriage return character |
| S4 | 10 | Line feed character |
| S5 | 8 | Backspace character |
| S6 | 2 | Wait for dial tone (seconds) |
| S7 | 30 | Connection timeout (seconds) |
| S8 | 2 | Comma pause time (seconds) |
| S9 | 6 | Carrier detect response time (1/10 sec) |
| S10 | 7 | Carrier loss delay (1/10 sec) |
| S12 | 50 | Escape guard time (1/50 sec) |

### Speed locking (AT&N)

The `AT&N` command locks the modem's connection speed, matching the real Hayes extended command set. This is used with phonebook entries that require a specific baud rate.

| Command | Speed |
|---------|-------|
| `AT&N0` | Auto (default, effectively 56000) |
| `AT&N1` | 300 |
| `AT&N2` | 1200 |
| `AT&N3` | 2400 |
| `AT&N6` | 4800 |
| `AT&N7` | 7200 |
| `AT&N8` | 9600 |
| `AT&N10` | 14400 |
| `AT&N11` | 19200 |
| `AT&N12` | 38400 |
| `AT&N14` | 56000 |

The locked speed is shown in `AT&V` output and reset to auto by `ATZ` or `AT&F`.

### Error correction and compression (AT&Q, AT%C)

At speeds >= 2400, real modems negotiate V.42 error correction (LAPM) and V.42bis data compression. The modem defaults to error correction on (`&Q5`) and compression on (`%C1`).

| Command | Description |
|---------|-------------|
| `AT&Q5` | Enable V.42/LAPM error correction (default) |
| `AT&Q0` | Disable error correction |
| `AT%C1` | Enable V.42bis compression (default) |
| `AT%C0` | Disable compression |

When error correction is active and the connection speed is >= 2400, the CONNECT string includes protocol information:

```
CONNECT 9600/ARQ/V42/LAPM/V42BIS   (error correction + compression)
CONNECT 9600/ARQ/V42/LAPM          (error correction only)
CONNECT 9600                        (no error correction)
```

These settings are shown in `AT&V` output and reset to defaults by `ATZ` or `AT&F`.

### Escape sequence

While connected (data mode), send `+++` with a pause before and after (S12 guard time, default 1 second) to return to command mode. Then use `ATH` to hang up or `ATO` to return to data mode.

## Example session

```
OK
ATZ
OK
AT&N8
OK
ATDT916-555-1212

RING
RING

CONNECT 9600/ARQ/V42/LAPM/V42BIS
```

Without the `AT&N8` (when the entry requires baud 9600), the user would see:

```
ATDT916-555-1212

RING
RING

CONNECT 28800
▒▓░█▒▓▒░▓█▒░...
NO CARRIER
```

## File Transfers (ZMODEM)

The web terminal supports ZMODEM upload and download when connected to a BBS
that offers `sz` / `rz`.

### Downloading

At the BBS prompt, run `sz <filename>` (or use the BBS's download menu). The
browser detects the ZMODEM header, receives the file into memory, and shows a
"Save" notification in the top-right corner. Click Save to trigger a standard
browser download. Batch transfers (multiple files per session) are supported —
each file gets its own notification.

### Uploading

At the BBS prompt, run `rz`. The browser detects the ZRINIT header and opens
a file picker. Select one or more files (up to `MAX_UPLOAD_BYTES` per file,
default 1 GB). Transfers stream directly from the browser to the BBS with no
server-side buffering.

### Configuration

Environment variables on the `web/server.js` process:

| Var | Default | Purpose |
|-----|---------|---------|
| `MAX_UPLOAD_BYTES` | `1073741824` (1 GB) | Client-side upload size cap. |
| `ZMODEM_TIMEOUT_SEC` | `30` | Session idle timeout. |

### Aborting

Press Ctrl-X five times to abort a transfer (standard ZMODEM cancel sequence).

## Web client security

**Third-party code is pinned.** xterm and its WebGL addon load from jsdelivr
with an `integrity` hash and `crossorigin="anonymous"`, so a compromised CDN
can serve a different file but the browser will refuse to run it. The URLs
point at the files npm actually publishes (`lib/xterm.js`, `css/xterm.css`) —
*not* jsdelivr's `.min.` variants, which jsdelivr generates on demand and which
therefore cannot be checked against the registry. After a version bump,
re-derive and verify against npm:

```sh
curl -s https://cdn.jsdelivr.net/npm/@xterm/xterm@<ver>/lib/xterm.js \
  | openssl dgst -sha384 -binary | openssl base64 -A
```

**Every response carries security headers** (`web/server.js`):

| Header | Value |
|--------|-------|
| `Content-Security-Policy` | `default-src 'self'`, scripts from self + jsdelivr only, `frame-ancestors`/`base-uri`/`object-src`/`form-action` all `'none'` |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |
| `Permissions-Policy` | camera, microphone, geolocation and friends all disabled |
| `Cross-Origin-Opener-Policy` / `-Resource-Policy` | `same-origin` |
| `Strict-Transport-Security` | `max-age=15552000; includeSubDomains` — **only on requests that arrived over TLS** |

The CSP allows `'unsafe-inline'` for **styles only**: xterm builds a `<style>`
element at runtime for its cell metrics and takes no nonce. Inline *script* is
not allowed, and there is none in the app.

HSTS is gated on the request actually being HTTPS — directly, or via
`X-Forwarded-Proto` from the nginx/Cloudflare front (`nginx/nginx.conf` sets
it). A plain-HTTP deployment therefore never promises TLS it does not have.

| Var | Default | Purpose |
|-----|---------|---------|
| `HSTS_MAX_AGE` | `15552000` (180 days) | HSTS lifetime, in seconds. |
| `MAX_WS_PER_IP` | `5` | Concurrent WebSocket connections per client IP. |

## Testing

```sh
make test        # Go unit tests
make web-test    # Node proxy + browser module unit tests
make web-e2e     # ZMODEM end-to-end test (requires lrzsz + playwright-cli)
```

## License

No License is granted. Not for distribution.

## TODO
- Secure/SSH access
- Web-based Terminal application
- ?
