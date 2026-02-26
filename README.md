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
# Build and run with Docker Compose (includes fail2ban)
make docker-up

# Or just the container
make docker
docker run -p 2323:2323 -v ./configs/telix.yaml:/etc/telix/telix.yaml:ro telix
```

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

## Testing

```sh
make test
# or
go test ./...
```

## License

No License is granted. Not for distribution.

## TODO
- Secure/SSH access
- Web-based Terminal application
- ?
