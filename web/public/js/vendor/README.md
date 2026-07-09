# Vendored libraries

## zmodem.min.js

**Source:** https://github.com/FGasper/zmodemjs (MIT)
**Version:** 0.1.10 (`zmodem.devel.js` from CDN)

Loaded via `<script>` tag; exposes `window.Zmodem`.

Key API used by this project:

- `new Zmodem.Sentry({ to_terminal, sender, on_detect, on_retract })`
- `sentry.consume(uint8Array)` — feed inbound bytes; the sentry either forwards
  them to `to_terminal(octets)` or (on detecting a ZMODEM header) invokes
  `on_detect(detection)`. Call `detection.confirm()` to obtain a session.
- Session events (`session.on('offer', xfer)`, `session.on('session_end', ...)`)
  and offer methods (`xfer.on('input', ...)`, `xfer.accept()`, `xfer.send(...)`,
  `xfer.get_details()`, `xfer.skip()`) drive per-file transfer.

Full API: <https://github.com/FGasper/zmodemjs/blob/master/README.md>
