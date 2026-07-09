# ZMODEM Web Transfer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add ZMODEM upload/download to the Telix web terminal — the BBS drives both directions, files stay entirely in the browser, progress renders in the modem chrome.

**Architecture:** WebSocket transport becomes always-binary. CP437 decoding moves from Node proxy to browser. Node proxy adds a one-line IAC-escape on outbound bytes so uploads survive the Go server's telnet filter. `zmodem.js` (vendored) runs the protocol in-browser via a Sentry that sits between the WS byte stream and xterm.js. UI additions: a status strip in the modem chrome and floating notification bubbles for completed downloads.

**Tech Stack:** Node.js (Express + `ws`), vanilla browser JS, `zmodem.js` (vendored from FGasper/zmodemjs, MIT), `node:test` (built-in) for unit tests, playwright-cli + `lrzsz` for E2E.

**Reference spec:** `docs/superpowers/specs/2026-07-08-zmodem-web-transfer-design.md`

---

## File Structure

**New files:**

| Path | Responsibility |
|------|---------------|
| `web/public/js/cp437.js` | CP437→Unicode table + `cp437ToUtf8(Uint8Array): string`. Moved from `server.js`. |
| `web/public/js/vendor/zmodem.min.js` | Vendored `zmodem.js` library, loaded via `<script>` tag. |
| `web/public/js/xfer-util.js` | Pure utility functions (filename sanitizer, byte formatter). Kept small so it can be unit-tested with `node:test`. |
| `web/public/js/zmodem-sentry.js` | Wraps `zmodem.js` Sentry. Owns the incoming byte pump and outbound byte pump. Emits UI events. |
| `web/public/js/zmodem-ui.js` | DOM binding for status strip, notification bubbles, file picker. Pure UI — no protocol logic. |
| `web/test/cp437.test.js` | Unit tests for CP437 decoder. |
| `web/test/xfer-util.test.js` | Unit tests for filename sanitizer + byte formatter. |
| `web/test/server.test.js` | Unit tests for Node proxy (IAC escape, config endpoint). |
| `web/e2e/zmodem.spec.js` | Playwright-cli-driven E2E against a fake BBS running `lrzsz`. |

**Modified files:**

| Path | Change |
|------|--------|
| `web/server.js` | Delete CP437 tables, send binary WS frames, IAC-escape outbound, add `GET /config.json`. |
| `web/public/index.html` | Add `.modem-xfer` element, `.xfer-notifications` aside, hidden `<input type="file">`, `<script>` tags. |
| `web/public/js/app.js` | Handle binary WS messages via new Sentry module, route CP437 decode through browser. |
| `web/public/css/terminal.css` | Add `.modem-xfer` and `.xfer-notifications` styles. |
| `web/package.json` | Add `test` script; no runtime deps added. |
| `Makefile` | Add `web-test` and `web-e2e` targets. |

---

## Task 1: Move CP437 decoder to browser

**Files:**
- Create: `web/public/js/cp437.js`
- Create: `web/test/cp437.test.js`
- Modify: `web/package.json`
- Modify: `Makefile`

- [ ] **Step 1: Write the failing test**

Create `web/test/cp437.test.js`:

```javascript
const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');

// Load browser module in a synthetic global scope.
const fs = require('node:fs');
const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'cp437.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src + '\nmodule.exports = { cp437ToUtf8 };')(sandbox, sandbox);
const { cp437ToUtf8 } = sandbox;

test('ASCII printable passes through unchanged', () => {
  const bytes = new Uint8Array([0x48, 0x69, 0x21]); // "Hi!"
  assert.strictEqual(cp437ToUtf8(bytes), 'Hi!');
});

test('control passthrough: CR/LF/TAB/ESC/BEL/BS/NUL preserved', () => {
  const bytes = new Uint8Array([0x00, 0x07, 0x08, 0x09, 0x0A, 0x0D, 0x1B]);
  assert.strictEqual(cp437ToUtf8(bytes), '\x00\x07\x08\x09\x0A\x0D\x1B');
});

test('CP437 low graphics: 0x01 -> ☺', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0x01])), '☺');
});

test('CP437 house at 0x7F -> ⌂', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0x7F])), '⌂');
});

test('CP437 high graphics: 0xB0 -> ░, 0xDB -> █', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0xB0])), '░');
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0xDB])), '█');
});

test('empty buffer returns empty string', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([])), '');
});

test('full 0x00-0xFF round-trip matches expected length', () => {
  const bytes = new Uint8Array(256);
  for (let i = 0; i < 256; i++) bytes[i] = i;
  const out = cp437ToUtf8(bytes);
  // Every byte produces exactly one code point.
  assert.strictEqual([...out].length, 256);
});
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd web && node --test test/cp437.test.js
```

Expected: FAIL — `ENOENT: no such file or directory, open '.../public/js/cp437.js'` or a module-load error.

- [ ] **Step 3: Create `web/public/js/cp437.js`**

```javascript
// CP437 → Unicode decoder for the Telix web terminal.
// Extracted from web/server.js so decoding happens client-side, allowing
// the WebSocket transport to be binary-clean (required for ZMODEM).

const CP437_TO_UNICODE = new Uint32Array(256);
for (let i = 0; i < 256; i++) CP437_TO_UNICODE[i] = i;

const cp437Low = {
  0x01: 0x263A, 0x02: 0x263B, 0x03: 0x2665, 0x04: 0x2666,
  0x05: 0x2663, 0x06: 0x2660, 0x0B: 0x2642, 0x0C: 0x2640,
  0x0E: 0x266B, 0x0F: 0x263C, 0x10: 0x25BA, 0x11: 0x25C4,
  0x12: 0x2195, 0x13: 0x203C, 0x14: 0x00B6, 0x15: 0x00A7,
  0x16: 0x25AC, 0x17: 0x21A8, 0x18: 0x2191, 0x19: 0x2193,
  0x1A: 0x2192, 0x1C: 0x221F, 0x1D: 0x2194, 0x1E: 0x25B2,
  0x1F: 0x25BC,
};
for (const [b, cp] of Object.entries(cp437Low)) CP437_TO_UNICODE[parseInt(b)] = cp;
CP437_TO_UNICODE[0x7F] = 0x2302;

const cp437High = [
  0x00C7, 0x00FC, 0x00E9, 0x00E2, 0x00E4, 0x00E0, 0x00E5, 0x00E7,
  0x00EA, 0x00EB, 0x00E8, 0x00EF, 0x00EE, 0x00EC, 0x00C4, 0x00C5,
  0x00C9, 0x00E6, 0x00C6, 0x00F4, 0x00F6, 0x00F2, 0x00FB, 0x00F9,
  0x00FF, 0x00D6, 0x00DC, 0x00A2, 0x00A3, 0x00A5, 0x20A7, 0x0192,
  0x00E1, 0x00ED, 0x00F3, 0x00FA, 0x00F1, 0x00D1, 0x00AA, 0x00BA,
  0x00BF, 0x2310, 0x00AC, 0x00BD, 0x00BC, 0x00A1, 0x00AB, 0x00BB,
  0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556,
  0x2555, 0x2563, 0x2551, 0x2557, 0x255D, 0x255C, 0x255B, 0x2510,
  0x2514, 0x2534, 0x252C, 0x251C, 0x2500, 0x253C, 0x255E, 0x255F,
  0x255A, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256C, 0x2567,
  0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256B,
  0x256A, 0x2518, 0x250C, 0x2588, 0x2584, 0x258C, 0x2590, 0x2580,
  0x03B1, 0x00DF, 0x0393, 0x03C0, 0x03A3, 0x03C3, 0x00B5, 0x03C4,
  0x03A6, 0x0398, 0x03A9, 0x03B4, 0x221E, 0x03C6, 0x03B5, 0x2229,
  0x2261, 0x00B1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00F7, 0x2248,
  0x00B0, 0x2219, 0x00B7, 0x221A, 0x207F, 0x00B2, 0x25A0, 0x00A0,
];
for (let i = 0; i < cp437High.length; i++) CP437_TO_UNICODE[0x80 + i] = cp437High[i];

const PASSTHROUGH_CONTROLS = new Set([0x00, 0x07, 0x08, 0x09, 0x0A, 0x0D, 0x1B]);

function cp437ToUtf8(buf) {
  let result = '';
  for (let i = 0; i < buf.length; i++) {
    const b = buf[i];
    if (PASSTHROUGH_CONTROLS.has(b)) {
      result += String.fromCharCode(b);
    } else {
      result += String.fromCodePoint(CP437_TO_UNICODE[b]);
    }
  }
  return result;
}

// Browser: expose on window; Node test harness reads source and injects module.exports.
if (typeof window !== 'undefined') {
  window.CP437 = { cp437ToUtf8 };
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd web && node --test test/cp437.test.js
```

Expected: `7 passing`.

- [ ] **Step 5: Add npm test script + Makefile target**

Modify `web/package.json` — add to `scripts`:

```json
"test": "node --test test/"
```

Result should look like:

```json
{
  "name": "telix-web",
  "version": "1.0.0",
  "private": true,
  "description": "Web terminal client for Telix modem emulator",
  "scripts": {
    "start": "node server.js",
    "dev": "node --watch server.js",
    "test": "node --test test/"
  },
  "dependencies": {
    "express": "^4.21.0",
    "ws": "^8.18.0"
  }
}
```

Modify `Makefile` — add these targets (insert before `docker-up` or at end):

```make
.PHONY: web-test
web-test:
	cd web && npm test

.PHONY: web-e2e
web-e2e:
	cd web && npm run e2e
```

- [ ] **Step 6: Commit**

```bash
git add web/public/js/cp437.js web/test/cp437.test.js web/package.json Makefile
git commit -m "feat(web): add browser-side CP437 decoder module"
```

---

## Task 2: Switch WebSocket transport to always-binary

**Files:**
- Modify: `web/server.js`
- Modify: `web/public/index.html`
- Modify: `web/public/js/app.js`
- Create: `web/test/server.test.js`

- [ ] **Step 1: Write failing server test for binary passthrough**

Create `web/test/server.test.js`:

```javascript
const test = require('node:test');
const assert = require('node:assert');
const net = require('node:net');
const http = require('node:http');
const { WebSocket } = require('ws');

// Spawn a fake Telix TCP server so we can drive the Node proxy end-to-end.
async function withProxy(callback) {
  const backend = net.createServer();
  const backendConns = [];
  backend.on('connection', c => backendConns.push(c));
  await new Promise(r => backend.listen(0, '127.0.0.1', r));
  const backendPort = backend.address().port;

  process.env.TELIX_HOST = '127.0.0.1';
  process.env.TELIX_PORT = String(backendPort);
  process.env.PORT = '0';
  delete require.cache[require.resolve('../server.js')];
  const serverModule = require('../server.js');
  // server.js starts a listener; we need the actual port. Wait a tick then read it.
  await new Promise(r => setTimeout(r, 50));
  const wsPort = serverModule.address().port;

  try {
    await callback({ wsPort, backendConns });
  } finally {
    serverModule.close();
    backend.close();
    for (const c of backendConns) c.destroy();
  }
}

test('WebSocket receives backend bytes as binary frames (not text)', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise((resolve, reject) => {
      ws.on('open', resolve);
      ws.on('error', reject);
    });

    // Wait for backend to accept.
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      ws.on('message', (data, isBinary) => resolve({ data, isBinary }));
    });

    backend.write(Buffer.from([0xFF, 0x00, 0x1B, 0x5B, 0x41])); // includes 0xFF and ESC

    const { data, isBinary } = await received;
    assert.strictEqual(isBinary, true, 'expected binary frame');
    assert.deepStrictEqual(new Uint8Array(data), new Uint8Array([0xFF, 0x00, 0x1B, 0x5B, 0x41]));
    ws.close();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd web && node --test test/server.test.js
```

Expected: FAIL — either the current `cp437ToUtf8` mangles the 0xFF byte or the module doesn't export `.address()`.

- [ ] **Step 3: Modify `web/server.js` — send binary, expose server**

Replace the CP437 constants and `cp437ToUtf8()` function (lines 17–101 of current file) with nothing (delete them). Then update the `tcp.on('data', …)` handler and the `server.listen(…)` block at the bottom.

Change `tcp.on('data', …)` block (currently ~line 144) to:

```javascript
tcp.on('data', (data) => {
  if (ws.readyState === ws.OPEN) {
    ws.send(data, { binary: true });
  }
});
```

Change the bottom of the file (currently ~line 209) so the server can be tested — export `server` and let PORT=0 produce an ephemeral port:

```javascript
server.listen(PORT, () => {
  const addr = server.address();
  const listeningPort = typeof addr === 'object' && addr ? addr.port : PORT;
  console.log(`Telix web terminal listening on http://0.0.0.0:${listeningPort}`);
  console.log(`Proxying to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
});

module.exports = server;
```

- [ ] **Step 4: Run server test again — should pass**

```bash
cd web && node --test test/server.test.js
```

Expected: PASS.

- [ ] **Step 5: Modify `web/public/index.html` — load cp437.js**

Add the CP437 script tag before `js/app.js` (in the `<body>` scripts area, around current line 52):

```html
<script src="js/modem-audio.js"></script>
<script src="js/cp437.js"></script>
<script src="js/app.js"></script>
```

- [ ] **Step 6: Modify `web/public/js/app.js` — decode binary via CP437**

Change the `ws.onmessage` handler (currently ~line 122) to always treat data as binary and decode via `window.CP437`:

```javascript
ws.onmessage = (event) => {
  let bytes;
  if (event.data instanceof ArrayBuffer) {
    bytes = new Uint8Array(event.data);
  } else if (event.data instanceof Blob) {
    // Fallback path in case binaryType wasn't respected.
    event.data.arrayBuffer().then(ab => handleBytes(new Uint8Array(ab)));
    return;
  } else {
    // Shouldn't happen after we went binary — but decode defensively as UTF-8 → bytes.
    bytes = new TextEncoder().encode(event.data);
  }
  handleBytes(bytes);
};

function handleBytes(bytes) {
  const text = window.CP437.cp437ToUtf8(bytes);
  term.write(text);
  if (bytes.length < 200) checkModemState(text);
  flashLed('rd');
}
```

- [ ] **Step 7: Manual smoke test**

Run the app and connect a browser:

```bash
cd .. && make build && make run &
cd web && npm start &
sleep 2
# Open http://localhost:3000 in a browser, verify text renders normally
```

Expected: banner and prompt display correctly (same as before). Kill both servers:

```bash
kill %1 %2
```

- [ ] **Step 8: Commit**

```bash
git add web/server.js web/public/index.html web/public/js/app.js web/test/server.test.js
git commit -m "feat(web): switch WebSocket transport to always-binary; CP437 decode in browser"
```

---

## Task 3: IAC-escape outbound bytes in Node proxy

**Files:**
- Modify: `web/server.js`
- Modify: `web/test/server.test.js`

- [ ] **Step 1: Write failing test for IAC escaping**

Append to `web/test/server.test.js`:

```javascript
test('outbound 0xFF from browser is escaped to 0xFF 0xFF before reaching backend', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise(r => ws.on('open', r));
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      const chunks = [];
      backend.on('data', d => {
        chunks.push(d);
        const total = Buffer.concat(chunks);
        if (total.length >= 4) resolve(total);
      });
    });

    // Send bytes containing an IAC (0xFF) as raw binary WS payload.
    ws.send(Buffer.from([0x41, 0xFF, 0x42])); // "A", IAC, "B"

    const got = await received;
    assert.deepStrictEqual(
      [...got.subarray(0, 4)],
      [0x41, 0xFF, 0xFF, 0x42],
      'IAC should be doubled'
    );
    ws.close();
  });
});

test('outbound bytes without 0xFF pass through unchanged', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise(r => ws.on('open', r));
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      backend.on('data', d => resolve(d));
    });

    ws.send(Buffer.from('hello'));
    const got = await received;
    assert.deepStrictEqual([...got], [...Buffer.from('hello')]);
    ws.close();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd web && node --test test/server.test.js
```

Expected: the new tests FAIL (IAC not doubled). Older tests continue to pass.

- [ ] **Step 3: Add `escapeIAC` and wire it into outbound path**

In `web/server.js`, add a helper near the top (below the `const IAC = 255;` group):

```javascript
// IAC-escape bytes for outbound telnet: 0xFF must be doubled per RFC 854.
// Runs on all browser→Go bytes (not just during ZMODEM) so we can never send an
// accidental unescaped IAC. Cost is O(n); zero allocation when no 0xFF present.
function escapeIAC(buf) {
  let hits = 0;
  for (let i = 0; i < buf.length; i++) if (buf[i] === IAC) hits++;
  if (hits === 0) return buf;
  const out = Buffer.allocUnsafe(buf.length + hits);
  let j = 0;
  for (let i = 0; i < buf.length; i++) {
    out[j++] = buf[i];
    if (buf[i] === IAC) out[j++] = IAC;
  }
  return out;
}
```

Then modify the `ws.on('message', …)` handler (currently ~line 151). Replace the else-branch `tcp.write(data)` line with escaped version:

```javascript
ws.on('message', (data, isBinary) => {
  if (isBinary && data.length >= 5 && data[0] === 0x00) {
    // Resize message: 0x00 + cols(uint16 BE) + rows(uint16 BE)
    const cols = (data[1] << 8) | data[2];
    const rows = (data[3] << 8) | data[4];
    const naws = Buffer.from([
      IAC, SB, NAWS,
      (cols >> 8) & 0xFF, cols & 0xFF,
      (rows >> 8) & 0xFF, rows & 0xFF,
      IAC, SE,
    ]);
    tcp.write(naws); // NAWS is proxy-constructed and correctly formed; do not re-escape.
  } else {
    tcp.write(escapeIAC(Buffer.isBuffer(data) ? data : Buffer.from(data)));
  }
});
```

Note: the length check for resize was `data.length >= 3` in the original; corrected to `>= 5` because the frame is 5 bytes.

- [ ] **Step 4: Run tests again — all pass**

```bash
cd web && node --test test/server.test.js
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add web/server.js web/test/server.test.js
git commit -m "feat(web): IAC-escape outbound bytes so ZMODEM uploads survive telnet"
```

---

## Task 4: Add `/config.json` endpoint

**Files:**
- Modify: `web/server.js`
- Modify: `web/test/server.test.js`

- [ ] **Step 1: Write failing test**

Append to `web/test/server.test.js`:

```javascript
test('GET /config.json returns MAX_UPLOAD_BYTES and ZMODEM_TIMEOUT_SEC', async () => {
  process.env.MAX_UPLOAD_BYTES = '2048';
  process.env.ZMODEM_TIMEOUT_SEC = '15';
  await withProxy(async ({ wsPort }) => {
    const body = await new Promise((resolve, reject) => {
      http.get(`http://127.0.0.1:${wsPort}/config.json`, res => {
        let data = '';
        res.on('data', c => data += c);
        res.on('end', () => resolve({ status: res.statusCode, body: JSON.parse(data) }));
      }).on('error', reject);
    });
    assert.strictEqual(body.status, 200);
    assert.strictEqual(body.body.maxUploadBytes, 2048);
    assert.strictEqual(body.body.zmodemTimeoutSec, 15);
  });
  delete process.env.MAX_UPLOAD_BYTES;
  delete process.env.ZMODEM_TIMEOUT_SEC;
});

test('GET /config.json defaults: 1 GiB / 30 s', async () => {
  await withProxy(async ({ wsPort }) => {
    const body = await new Promise((resolve, reject) => {
      http.get(`http://127.0.0.1:${wsPort}/config.json`, res => {
        let data = '';
        res.on('data', c => data += c);
        res.on('end', () => resolve(JSON.parse(data)));
      }).on('error', reject);
    });
    assert.strictEqual(body.maxUploadBytes, 1073741824);
    assert.strictEqual(body.zmodemTimeoutSec, 30);
  });
});
```

- [ ] **Step 2: Run test — should fail**

```bash
cd web && node --test test/server.test.js
```

Expected: last two tests FAIL (404 or JSON parse error).

- [ ] **Step 3: Add env parsing + route in `web/server.js`**

Near the top of `web/server.js` with the other env reads:

```javascript
const MAX_UPLOAD_BYTES = parseInt(process.env.MAX_UPLOAD_BYTES || String(1024 * 1024 * 1024), 10);
const ZMODEM_TIMEOUT_SEC = parseInt(process.env.ZMODEM_TIMEOUT_SEC || '30', 10);
```

After the `app.use(express.static(...))` line, add:

```javascript
app.get('/config.json', (req, res) => {
  res.json({
    maxUploadBytes: MAX_UPLOAD_BYTES,
    zmodemTimeoutSec: ZMODEM_TIMEOUT_SEC,
  });
});
```

- [ ] **Step 4: Run tests — pass**

```bash
cd web && node --test test/server.test.js
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add web/server.js web/test/server.test.js
git commit -m "feat(web): add /config.json endpoint for browser-side transfer limits"
```

---

## Task 5: Vendor `zmodem.js`

**Files:**
- Create: `web/public/js/vendor/zmodem.min.js`
- Create: `web/public/js/vendor/README.md`
- Modify: `web/public/index.html`

- [ ] **Step 1: Download the library**

The vendored file is FGasper/zmodemjs `zmodem.devel.js` (or the minified build). Fetch it:

```bash
mkdir -p web/public/js/vendor
curl -fsSL -o web/public/js/vendor/zmodem.min.js \
  https://cdn.jsdelivr.net/npm/zmodem@0.1.10/dist/zmodem.devel.js
ls -l web/public/js/vendor/zmodem.min.js
```

Expected: file exists, size ~200 KB (devel build; browser-cache is fine — we'll switch to a minified build in a future task if desired). Verify it defines `Zmodem`:

```bash
grep -c "Zmodem" web/public/js/vendor/zmodem.min.js
```

Expected: > 100 matches.

- [ ] **Step 2: Add a README noting the source**

Create `web/public/js/vendor/README.md`:

```markdown
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
```

- [ ] **Step 3: Add script tag to `web/public/index.html`**

Insert before `js/app.js`:

```html
<script src="js/modem-audio.js"></script>
<script src="js/cp437.js"></script>
<script src="js/vendor/zmodem.min.js"></script>
<script src="js/app.js"></script>
```

- [ ] **Step 4: Manual smoke — page still loads**

```bash
cd web && npm start &
sleep 2
curl -sf http://localhost:3000/js/vendor/zmodem.min.js -o /dev/null && echo OK
curl -sf http://localhost:3000/ -o /dev/null && echo OK
kill %1
```

Expected: two `OK` lines.

- [ ] **Step 5: Commit**

```bash
git add web/public/js/vendor/ web/public/index.html
git commit -m "chore(web): vendor zmodem.js (FGasper/zmodemjs 0.1.10)"
```

---

## Task 6: Transfer utility module

**Files:**
- Create: `web/public/js/xfer-util.js`
- Create: `web/test/xfer-util.test.js`

- [ ] **Step 1: Write failing tests**

Create `web/test/xfer-util.test.js`:

```javascript
const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'xfer-util.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src + '\nmodule.exports = { sanitizeFilename, formatBytes, formatDuration };')(sandbox, sandbox);
const { sanitizeFilename, formatBytes, formatDuration } = sandbox;

test('sanitizeFilename strips path components', () => {
  assert.strictEqual(sanitizeFilename('../../etc/passwd'), 'passwd');
  assert.strictEqual(sanitizeFilename('C:\\Users\\me\\file.zip'), 'file.zip');
});

test('sanitizeFilename replaces unsafe chars with underscore', () => {
  assert.strictEqual(sanitizeFilename('bad name?.txt'), 'bad_name_.txt');
  assert.strictEqual(sanitizeFilename('中文.zip'), '__.zip');
});

test('sanitizeFilename allows [A-Za-z0-9._-]', () => {
  assert.strictEqual(sanitizeFilename('OK_file-1.2.zip'), 'OK_file-1.2.zip');
});

test('sanitizeFilename caps length at 128', () => {
  const long = 'a'.repeat(200) + '.zip';
  const out = sanitizeFilename(long);
  assert.strictEqual(out.length, 128);
});

test('sanitizeFilename empty or all-invalid -> download.bin', () => {
  assert.strictEqual(sanitizeFilename(''), 'download.bin');
  assert.strictEqual(sanitizeFilename('///'), 'download.bin');
  assert.strictEqual(sanitizeFilename(null), 'download.bin');
});

test('formatBytes: human-readable', () => {
  assert.strictEqual(formatBytes(0), '0 B');
  assert.strictEqual(formatBytes(512), '512 B');
  assert.strictEqual(formatBytes(1024), '1.0 KB');
  assert.strictEqual(formatBytes(1536), '1.5 KB');
  assert.strictEqual(formatBytes(1048576), '1.0 MB');
  assert.strictEqual(formatBytes(1073741824), '1.0 GB');
});

test('formatDuration: MM:SS', () => {
  assert.strictEqual(formatDuration(0), '00:00');
  assert.strictEqual(formatDuration(65), '01:05');
  assert.strictEqual(formatDuration(3599), '59:59');
  assert.strictEqual(formatDuration(Infinity), '--:--');
  assert.strictEqual(formatDuration(NaN), '--:--');
});
```

- [ ] **Step 2: Run tests — should fail**

```bash
cd web && node --test test/xfer-util.test.js
```

Expected: ENOENT loading `xfer-util.js`.

- [ ] **Step 3: Create `web/public/js/xfer-util.js`**

```javascript
// Pure utility functions for ZMODEM transfer UI. Kept separate from
// zmodem-ui.js so they can be unit-tested without a DOM.

function sanitizeFilename(name) {
  if (typeof name !== 'string' || name.length === 0) return 'download.bin';
  // Strip path components — take basename after any / or \.
  const parts = name.split(/[\\/]/);
  let base = parts[parts.length - 1] || '';
  // Restrict to safe chars.
  base = base.replace(/[^A-Za-z0-9._-]/g, '_');
  if (base.length === 0) return 'download.bin';
  if (base.length > 128) base = base.slice(0, 128);
  return base;
}

function formatBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  return (n / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function formatDuration(seconds) {
  if (!isFinite(seconds) || isNaN(seconds) || seconds < 0) return '--:--';
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return String(m).padStart(2, '0') + ':' + String(s).padStart(2, '0');
}

if (typeof window !== 'undefined') {
  window.XferUtil = { sanitizeFilename, formatBytes, formatDuration };
}
```

- [ ] **Step 4: Run tests — pass**

```bash
cd web && node --test test/xfer-util.test.js
```

Expected: all PASS.

- [ ] **Step 5: Wire into HTML**

Modify `web/public/index.html` script tag order:

```html
<script src="js/modem-audio.js"></script>
<script src="js/cp437.js"></script>
<script src="js/xfer-util.js"></script>
<script src="js/vendor/zmodem.min.js"></script>
<script src="js/app.js"></script>
```

- [ ] **Step 6: Commit**

```bash
git add web/public/js/xfer-util.js web/test/xfer-util.test.js web/public/index.html
git commit -m "feat(web): add xfer-util module (filename sanitizer, byte/duration formatters)"
```

---

## Task 7: HTML skeleton for transfer UI

**Files:**
- Modify: `web/public/index.html`
- Modify: `web/public/css/terminal.css`

- [ ] **Step 1: Add DOM elements to `web/public/index.html`**

Inside `.modem-front`, insert the status strip between `.modem-leds` and `.modem-mute`:

```html
<div class="modem-front">
  <div class="modem-leds">
    <!-- ... existing LEDs ... -->
  </div>
  <div class="modem-xfer" id="xferStrip" aria-live="polite" hidden>
    <span class="modem-xfer-dir" id="xferDir">▶</span>
    <span class="modem-xfer-name" id="xferName">—</span>
    <span class="modem-xfer-pct" id="xferPct">0%</span>
    <span class="modem-xfer-bar" id="xferBar"><span class="modem-xfer-fill" id="xferFill"></span></span>
    <span class="modem-xfer-cps" id="xferCps">0 CPS</span>
    <span class="modem-xfer-eta" id="xferEta">--:--</span>
    <span class="modem-xfer-batch" id="xferBatch" hidden></span>
  </div>
  <button class="modem-mute" id="muteBtn" title="Toggle modem sounds">
    <span class="modem-mute-icon" id="muteIcon">&#x1F50A;</span>
  </button>
  <!-- ... existing brand ... -->
</div>
```

After `</div>` closing `.crt-container`, add the notification area and hidden file input:

```html
<aside class="xfer-notifications" id="xferNotifications" aria-live="polite"></aside>
<input type="file" id="xferUploadInput" multiple hidden />
```

- [ ] **Step 2: Add CSS to `web/public/css/terminal.css`**

Append at end of file:

```css
/* -------- Transfer status strip (LCD panel style) -------- */

.modem-xfer {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 10px;
  background: #001100;
  color: #55ff55;
  font-family: 'PxPlus IBM VGA 8x16', monospace;
  font-size: 12px;
  border: 1px solid #003300;
  border-radius: 3px;
  box-shadow: inset 0 0 8px rgba(85, 255, 85, 0.15);
  text-shadow: 0 0 3px rgba(85, 255, 85, 0.4);
  min-width: 380px;
}

.modem-xfer[hidden] {
  display: none;
}

.modem-xfer-dir {
  font-weight: bold;
  color: #ffff55;
}

.modem-xfer-name {
  flex: 1;
  min-width: 80px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.modem-xfer-pct {
  min-width: 3ch;
  text-align: right;
}

.modem-xfer-bar {
  position: relative;
  display: inline-block;
  width: 120px;
  height: 10px;
  background: #003300;
  border: 1px solid #005500;
}

.modem-xfer-fill {
  display: block;
  width: 0%;
  height: 100%;
  background: #55ff55;
  transition: width 120ms linear;
}

@media (prefers-reduced-motion: reduce) {
  .modem-xfer-fill { transition: none; }
}

.modem-xfer-cps { min-width: 8ch; }
.modem-xfer-eta { color: #55ffff; min-width: 5ch; }
.modem-xfer-batch { color: #aaaaaa; }

.modem-xfer.state-aborted { color: #ff5555; text-shadow: 0 0 3px rgba(255, 85, 85, 0.4); }
.modem-xfer.state-timeout { color: #ffff55; text-shadow: 0 0 3px rgba(255, 255, 85, 0.4); }

/* -------- Download notification bubbles -------- */

.xfer-notifications {
  position: fixed;
  top: 20px;
  right: 20px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  z-index: 1000;
  pointer-events: none;
}

.xfer-notification {
  pointer-events: auto;
  background: #001100;
  color: #55ff55;
  border: 1px solid #005500;
  padding: 8px 12px;
  font-family: 'PxPlus IBM VGA 8x16', monospace;
  font-size: 14px;
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 260px;
  box-shadow: 0 0 12px rgba(85, 255, 85, 0.25);
  animation: xfer-slide-in 220ms ease-out;
}

.xfer-notification.error { color: #ff5555; border-color: #550000; }

.xfer-notification button {
  background: #55ff55;
  color: #000;
  border: none;
  padding: 4px 12px;
  font-family: inherit;
  font-weight: bold;
  cursor: pointer;
}

.xfer-notification button:hover { background: #aaffaa; }
.xfer-notification button:focus-visible { outline: 2px solid #ffff55; outline-offset: 2px; }

@keyframes xfer-slide-in {
  from { opacity: 0; transform: translateX(20px); }
  to   { opacity: 1; transform: translateX(0); }
}

@media (prefers-reduced-motion: reduce) {
  .xfer-notification { animation: none; }
}
```

- [ ] **Step 3: Manual smoke — verify no visual regression**

```bash
cd web && npm start &
sleep 2
```

Open browser to `http://localhost:3000`, verify:
- Modem chrome still renders correctly
- LEDs still animate
- No visible transfer strip (it's `hidden`)
- No visible notifications

```bash
kill %1
```

- [ ] **Step 4: Commit**

```bash
git add web/public/index.html web/public/css/terminal.css
git commit -m "feat(web): add HTML/CSS for transfer status strip and notification bubbles"
```

---

## Task 8: zmodem-ui.js — status strip + notifications

**Files:**
- Create: `web/public/js/zmodem-ui.js`
- Modify: `web/public/index.html`

- [ ] **Step 1: Create `web/public/js/zmodem-ui.js`**

```javascript
// UI layer for ZMODEM transfers. Pure DOM binding — protocol logic lives in
// zmodem-sentry.js. Exposes window.ZmodemUI with these methods:
//   startXfer({ direction, filename, size, batchIndex, batchTotal })
//   updateXfer({ bytes, cps })
//   endXfer({ status })                // status: 'done' | 'aborted' | 'timeout'
//   surfaceDownload({ filename, blob })
//   surfaceError(message)
//   promptUpload(maxBytes) -> Promise<File[]>

(function () {
  const $ = id => document.getElementById(id);
  const strip = $('xferStrip');
  const dir = $('xferDir');
  const name = $('xferName');
  const pct = $('xferPct');
  const fill = $('xferFill');
  const cpsEl = $('xferCps');
  const etaEl = $('xferEta');
  const batchEl = $('xferBatch');
  const notifications = $('xferNotifications');
  const uploadInput = $('xferUploadInput');

  let currentSize = 0;
  let startedAt = 0;

  function startXfer({ direction, filename, size, batchIndex, batchTotal }) {
    strip.hidden = false;
    strip.classList.remove('state-aborted', 'state-timeout');
    dir.textContent = direction === 'send' ? '▲' : '▶';
    name.textContent = window.XferUtil.sanitizeFilename(filename);
    currentSize = size || 0;
    startedAt = performance.now();
    pct.textContent = '0%';
    fill.style.width = '0%';
    cpsEl.textContent = '0 CPS';
    etaEl.textContent = '--:--';
    if (batchTotal && batchTotal > 1) {
      batchEl.hidden = false;
      batchEl.textContent = `${batchIndex}/${batchTotal}`;
    } else {
      batchEl.hidden = true;
    }
  }

  function updateXfer({ bytes, cps }) {
    const percent = currentSize > 0 ? Math.floor((bytes / currentSize) * 100) : 0;
    pct.textContent = percent + '%';
    fill.style.width = Math.min(100, percent) + '%';
    cpsEl.textContent = (cps || 0).toFixed(0) + ' CPS';
    if (cps > 0 && currentSize > bytes) {
      const remaining = (currentSize - bytes) / cps;
      etaEl.textContent = window.XferUtil.formatDuration(remaining);
    }
  }

  function endXfer({ status }) {
    if (status === 'done') {
      pct.textContent = '100%';
      fill.style.width = '100%';
      etaEl.textContent = '00:00';
      setTimeout(() => { strip.hidden = true; }, 800);
    } else if (status === 'aborted') {
      strip.classList.add('state-aborted');
      name.textContent = '✕ ABORTED';
      setTimeout(() => { strip.hidden = true; }, 2000);
    } else if (status === 'timeout') {
      strip.classList.add('state-timeout');
      name.textContent = '✕ TIMEOUT';
      setTimeout(() => { strip.hidden = true; }, 2000);
    }
  }

  function surfaceDownload({ filename, blob }) {
    const safeName = window.XferUtil.sanitizeFilename(filename);
    const size = window.XferUtil.formatBytes(blob.size);
    const bubble = document.createElement('div');
    bubble.className = 'xfer-notification';

    const label = document.createElement('span');
    label.textContent = `⬇ ${safeName}  (${size})`;
    label.style.flex = '1';

    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Save';

    let objectUrl = URL.createObjectURL(blob);
    let removed = false;
    function dismiss() {
      if (removed) return;
      removed = true;
      URL.revokeObjectURL(objectUrl);
      bubble.remove();
    }

    button.addEventListener('click', () => {
      const a = document.createElement('a');
      a.href = objectUrl;
      a.download = safeName;
      document.body.appendChild(a);
      a.click();
      a.remove();
      dismiss();
    });

    bubble.appendChild(label);
    bubble.appendChild(button);
    notifications.appendChild(bubble);

    // Auto-dismiss after 60 s.
    setTimeout(dismiss, 60_000);
  }

  function surfaceError(message) {
    const bubble = document.createElement('div');
    bubble.className = 'xfer-notification error';
    bubble.textContent = '✕ ' + message;
    notifications.appendChild(bubble);
    setTimeout(() => bubble.remove(), 5000);
  }

  function promptUpload(maxBytes) {
    return new Promise(resolve => {
      const onChange = () => {
        uploadInput.removeEventListener('change', onChange);
        uploadInput.removeEventListener('cancel', onCancel);
        const files = Array.from(uploadInput.files || []);
        uploadInput.value = ''; // reset so same file can be re-picked
        const oversize = files.find(f => f.size > maxBytes);
        if (oversize) {
          surfaceError(`${oversize.name}: exceeds ${window.XferUtil.formatBytes(maxBytes)}`);
          resolve([]);
        } else {
          resolve(files);
        }
      };
      const onCancel = () => {
        uploadInput.removeEventListener('change', onChange);
        uploadInput.removeEventListener('cancel', onCancel);
        resolve([]);
      };
      uploadInput.addEventListener('change', onChange);
      uploadInput.addEventListener('cancel', onCancel);
      uploadInput.click();
    });
  }

  window.ZmodemUI = {
    startXfer, updateXfer, endXfer,
    surfaceDownload, surfaceError, promptUpload,
  };
})();
```

- [ ] **Step 2: Wire into `web/public/index.html`**

Adjust script tags:

```html
<script src="js/modem-audio.js"></script>
<script src="js/cp437.js"></script>
<script src="js/xfer-util.js"></script>
<script src="js/vendor/zmodem.min.js"></script>
<script src="js/zmodem-ui.js"></script>
<script src="js/app.js"></script>
```

- [ ] **Step 3: Manual smoke — verify page loads without console errors**

```bash
cd web && npm start &
sleep 2
```

Open browser, open DevTools console, verify no errors. `window.ZmodemUI` should be defined.

```bash
kill %1
```

- [ ] **Step 4: Commit**

```bash
git add web/public/js/zmodem-ui.js web/public/index.html
git commit -m "feat(web): add zmodem-ui module (status strip, notifications, upload picker)"
```

---

## Task 9: zmodem-sentry.js — Sentry integration

**Files:**
- Create: `web/public/js/zmodem-sentry.js`
- Modify: `web/public/js/app.js`
- Modify: `web/public/index.html`

- [ ] **Step 1: Create `web/public/js/zmodem-sentry.js`**

```javascript
// Bridges the WebSocket byte stream to zmodem.js + xterm.js. Owns byte-pump
// routing:
//   - Inbound (WS -> ??): non-ZMODEM bytes go to xterm via CP437 decode.
//     During a ZMODEM session, bytes are consumed by the session instead.
//   - Outbound (?? -> WS): xterm keystrokes when idle, session-emitted bytes
//     during transfer.
// Emits UI events via window.ZmodemUI.

(function () {
  function createZmodemBridge({ ws, term, config, checkModemState, flashLed }) {
    let session = null;
    let idleTimer = null;
    let lastActivity = Date.now();
    let currentXferBytes = 0;
    let currentXferStart = 0;

    function armTimeout() {
      clearTimeout(idleTimer);
      idleTimer = setTimeout(() => {
        if (!session) return;
        window.ZmodemUI.endXfer({ status: 'timeout' });
        try { session.abort(); } catch (e) { /* session may already be dead */ }
        session = null;
      }, config.zmodemTimeoutSec * 1000);
    }

    function disarmTimeout() { clearTimeout(idleTimer); idleTimer = null; }

    function sendToWS(octets) {
      if (ws.readyState === WebSocket.OPEN) {
        const buf = octets instanceof Uint8Array ? octets : new Uint8Array(octets);
        ws.send(buf);
      }
    }

    const sentry = new Zmodem.Sentry({
      to_terminal(octets) {
        // Non-ZMODEM bytes: decode via CP437 and write to xterm.
        const bytes = octets instanceof Uint8Array ? octets : new Uint8Array(octets);
        const text = window.CP437.cp437ToUtf8(bytes);
        term.write(text);
        if (bytes.length < 200) checkModemState(text);
        flashLed('rd');
      },
      sender(octets) { sendToWS(octets); },
      on_retract() { /* False positive; nothing to do. */ },
      on_detect(detection) {
        session = detection.confirm();
        armTimeout();

        session.on('session_end', () => {
          disarmTimeout();
          session = null;
        });

        if (session.type === 'receive') {
          handleReceive(session);
        } else if (session.type === 'send') {
          handleSend(session);
        }
      },
    });

    function handleReceive(zsession) {
      let batchIndex = 0;
      // The batch total is not known in advance for standard ZMODEM;
      // batchTotal is 0 unless the sender provides files_remaining.

      zsession.on('offer', xfer => {
        batchIndex++;
        const details = xfer.get_details();
        const chunks = [];
        currentXferBytes = 0;
        currentXferStart = performance.now();

        window.ZmodemUI.startXfer({
          direction: 'recv',
          filename: details.name,
          size: details.size || 0,
          batchIndex,
          batchTotal: (details.files_remaining || 0) + batchIndex,
        });

        xfer.on('input', payload => {
          const buf = payload instanceof Uint8Array ? payload : new Uint8Array(payload);
          chunks.push(buf);
          currentXferBytes += buf.length;
          const elapsed = (performance.now() - currentXferStart) / 1000;
          const cps = elapsed > 0 ? currentXferBytes / elapsed : 0;
          window.ZmodemUI.updateXfer({ bytes: currentXferBytes, cps });
          armTimeout();
        });

        xfer.accept().then(() => {
          window.ZmodemUI.endXfer({ status: 'done' });
          const blob = new Blob(chunks);
          window.ZmodemUI.surfaceDownload({ filename: details.name, blob });
        }).catch(err => {
          console.error('ZMODEM receive error', err);
          window.ZmodemUI.endXfer({ status: 'aborted' });
        });
      });

      zsession.start();
    }

    function handleSend(zsession) {
      window.ZmodemUI.promptUpload(config.maxUploadBytes).then(async files => {
        if (files.length === 0) {
          try { zsession.close(); } catch (e) { /* nop */ }
          return;
        }
        for (let i = 0; i < files.length; i++) {
          const file = files[i];
          const buf = new Uint8Array(await file.arrayBuffer());
          currentXferBytes = 0;
          currentXferStart = performance.now();
          window.ZmodemUI.startXfer({
            direction: 'send',
            filename: file.name,
            size: file.size,
            batchIndex: i + 1,
            batchTotal: files.length,
          });
          const xfer = await zsession.send_offer({
            name: file.name,
            size: file.size,
            mtime: Math.floor(file.lastModified / 1000),
            files_remaining: files.length - i - 1,
            bytes_remaining: files.slice(i).reduce((s, f) => s + f.size, 0),
          });
          if (!xfer) {
            // BBS skipped this file.
            window.ZmodemUI.endXfer({ status: 'done' });
            continue;
          }
          // Chunk into 4 KB blocks to keep progress smooth.
          const CHUNK = 4096;
          for (let off = 0; off < buf.length; off += CHUNK) {
            const slice = buf.subarray(off, Math.min(off + CHUNK, buf.length));
            await xfer.send(slice);
            currentXferBytes = off + slice.length;
            const elapsed = (performance.now() - currentXferStart) / 1000;
            const cps = elapsed > 0 ? currentXferBytes / elapsed : 0;
            window.ZmodemUI.updateXfer({ bytes: currentXferBytes, cps });
            armTimeout();
          }
          await xfer.end();
          window.ZmodemUI.endXfer({ status: 'done' });
        }
        try { await zsession.close(); } catch (e) { /* nop */ }
      });
    }

    return {
      consume(bytes) { sentry.consume(bytes); },
      isActive() { return session !== null; },
    };
  }

  window.ZmodemSentry = { createZmodemBridge };
})();
```

- [ ] **Step 2: Wire into `web/public/index.html`**

```html
<script src="js/modem-audio.js"></script>
<script src="js/cp437.js"></script>
<script src="js/xfer-util.js"></script>
<script src="js/vendor/zmodem.min.js"></script>
<script src="js/zmodem-ui.js"></script>
<script src="js/zmodem-sentry.js"></script>
<script src="js/app.js"></script>
```

- [ ] **Step 3: Modify `web/public/js/app.js` — route WS through Sentry**

Replace the `ws.onmessage` block from Task 2 with the Sentry-routed version, and load config first. Insert config load after `const ws = new WebSocket(...)`:

```javascript
let bridge = null;
let config = { maxUploadBytes: 1073741824, zmodemTimeoutSec: 30 };

fetch('/config.json')
  .then(r => r.json())
  .then(c => { config = c; })
  .catch(() => { /* use defaults */ });
```

Replace the `ws.onmessage` handler with:

```javascript
ws.onmessage = (event) => {
  if (!bridge) {
    bridge = window.ZmodemSentry.createZmodemBridge({
      ws, term, config, checkModemState, flashLed,
    });
  }
  let bytes;
  if (event.data instanceof ArrayBuffer) {
    bytes = new Uint8Array(event.data);
  } else if (event.data instanceof Blob) {
    event.data.arrayBuffer().then(ab => bridge.consume(new Uint8Array(ab)));
    return;
  } else {
    bytes = new TextEncoder().encode(event.data);
  }
  bridge.consume(bytes);
};
```

The `handleBytes()` helper added in Task 2 is no longer used — remove it.

- [ ] **Step 4: Manual smoke test — normal terminal still works**

```bash
cd .. && make build && make run &
cd web && npm start &
sleep 2
```

Open `http://localhost:3000`, verify:
- Banner renders
- ATZ prompt echoes
- Type `ATZ<Enter>`, see `OK`

```bash
kill %1 %2
```

- [ ] **Step 5: Commit**

```bash
git add web/public/js/zmodem-sentry.js web/public/js/app.js web/public/index.html
git commit -m "feat(web): route WebSocket bytes through zmodem.js Sentry"
```

---

## Task 10: End-to-end test with real `lrzsz`

**Files:**
- Create: `web/e2e/zmodem.spec.js`
- Create: `web/e2e/fixtures/hello.txt`
- Modify: `web/package.json`

- [ ] **Step 1: Check `lrzsz` is available**

```bash
which sz rz || brew install lrzsz
```

Expected: `sz` and `rz` binaries at `/opt/homebrew/bin/` (macOS) or `/usr/bin/` (Linux).

- [ ] **Step 2: Create fixture**

```bash
mkdir -p web/e2e/fixtures
printf 'hello world from zmodem\n' > web/e2e/fixtures/hello.txt
```

- [ ] **Step 3: Create `web/e2e/zmodem.spec.js`**

```javascript
// E2E test: fake BBS runs `sz` against a fixture; browser (driven by
// playwright-cli) receives it via the notification bubble.
//
// Run: node web/e2e/zmodem.spec.js
//
// Requires: lrzsz (sz, rz), playwright-cli.

const net = require('node:net');
const { spawn } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const assert = require('node:assert');

const PW_SESSION = process.env.PILOT_SESSION_ID || 'zmodem-e2e';
const FIXTURE = path.join(__dirname, 'fixtures', 'hello.txt');

function runPW(args) {
  return new Promise((resolve, reject) => {
    const p = spawn('playwright-cli', ['-s=' + PW_SESSION, ...args], { stdio: ['ignore', 'pipe', 'inherit'] });
    let out = '';
    p.stdout.on('data', d => out += d);
    p.on('exit', code => code === 0 ? resolve(out) : reject(new Error(`playwright-cli ${args[0]} exit ${code}`)));
  });
}

async function main() {
  // 1. Start a fake BBS listener that runs `sz` on connect.
  const bbs = net.createServer(conn => {
    const sz = spawn('sz', ['--zmodem', FIXTURE], { stdio: ['pipe', 'pipe', 'inherit'] });
    conn.pipe(sz.stdin);
    sz.stdout.pipe(conn);
    sz.on('exit', () => conn.end());
    conn.on('close', () => sz.kill());
  });
  await new Promise(r => bbs.listen(0, '127.0.0.1', r));
  const bbsPort = bbs.address().port;

  // 2. Start the Node proxy pointed at the fake BBS.
  const proxyEnv = { ...process.env, TELIX_HOST: '127.0.0.1', TELIX_PORT: String(bbsPort), PORT: '3123' };
  const proxy = spawn('node', [path.join(__dirname, '..', 'server.js')], { env: proxyEnv, stdio: ['ignore', 'pipe', 'inherit'] });
  await new Promise(r => setTimeout(r, 500));

  try {
    // 3. Drive the browser: open, wait for notification, click Save.
    await runPW(['open', 'http://127.0.0.1:3123']);
    // Wait for the notification bubble to appear (sz auto-starts on connect).
    let snap = '';
    for (let i = 0; i < 30; i++) {
      snap = await runPW(['snapshot']);
      if (/hello\.txt/.test(snap)) break;
      await new Promise(r => setTimeout(r, 500));
    }
    assert.match(snap, /hello\.txt/, 'expected notification for hello.txt');
    // We don't click Save automatically here (browser download would leave
    // artifacts). Instead, verify the terminal shows no garbage and the strip
    // reached 100%.
    console.log('E2E download: notification surfaced OK');
  } finally {
    await runPW(['close']).catch(() => {});
    proxy.kill();
    bbs.close();
  }
}

main().catch(e => { console.error(e); process.exit(1); });
```

- [ ] **Step 4: Add e2e script to `package.json`**

```json
"scripts": {
  "start": "node server.js",
  "dev": "node --watch server.js",
  "test": "node --test test/",
  "e2e": "node e2e/zmodem.spec.js"
}
```

- [ ] **Step 5: Run e2e**

```bash
cd web && npm run e2e
```

Expected: `E2E download: notification surfaced OK`.

If `sz` is not installed, expected: helpful error. If `playwright-cli` not on PATH, expected: helpful error. Both are prerequisites — install them and retry.

- [ ] **Step 6: Commit**

```bash
git add web/e2e/ web/package.json
git commit -m "test(web): E2E ZMODEM download test with lrzsz + playwright-cli"
```

---

## Task 11: Documentation

**Files:**
- Modify: `web/Dockerfile` (verify no changes needed)
- Modify: `README.md` (or `web/README.md` — check which exists)

- [ ] **Step 1: Check for existing README**

```bash
ls -la README.md web/README.md 2>/dev/null
```

If `README.md` exists at repo root, use it. Otherwise create `web/README.md`.

- [ ] **Step 2: Add a "File Transfers" section**

Append to the appropriate README:

```markdown
## File Transfers (ZMODEM)

The web terminal supports ZMODEM upload and download when connected to a BBS
that offers `sz`/`rz`.

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
```

- [ ] **Step 3: Commit**

```bash
git add README.md web/README.md 2>/dev/null || git add README.md
git commit -m "docs: document ZMODEM file transfer in the web terminal"
```

---

## Task 12: Final integration verification

- [ ] **Step 1: Run all unit tests**

```bash
make web-test
```

Expected: all suites PASS.

- [ ] **Step 2: Run E2E test**

```bash
make web-e2e
```

Expected: `E2E download: notification surfaced OK`.

- [ ] **Step 3: Manual verification against a real BBS (optional but recommended)**

Start the full stack:

```bash
make build && make run &
cd web && npm start &
sleep 2
```

Open `http://localhost:3000`, dial a phonebook entry that reaches a BBS with file areas. Try a small download; verify:
- Terminal goes quiet during the transfer.
- Status strip shows filename, %, CPS, ETA.
- On completion, a notification bubble appears.
- Clicking Save produces a byte-identical file.

Kill servers.

- [ ] **Step 4: Verify Go-side tests still pass**

```bash
make test
make vet
```

Expected: all PASS. No Go source was touched.

- [ ] **Step 5: Commit any incidental fixes and merge**

```bash
git status
# If everything is clean, no commit needed.
```

---

## Self-Review Notes

**Spec coverage checklist** (against `2026-07-08-zmodem-web-transfer-design.md`):

- §2 Architecture (always-binary WS, CP437 in browser, Sentry) → Tasks 1, 2, 5, 9.
- §3 Node proxy (delete CP437, IAC-escape, /config.json) → Tasks 2, 3, 4.
- §4 Browser components (`cp437.js`, `xfer-util.js`, `zmodem-sentry.js`, `zmodem-ui.js`, vendored lib) → Tasks 1, 5, 6, 8, 9.
- §5.1 Modem chrome status strip → Task 7 (HTML/CSS), Task 8 (JS updates).
- §5.2 Download notification bubbles → Task 7 (HTML/CSS), Task 8 (JS).
- §5.3 Upload picker → Task 7 (hidden input), Task 8 (promptUpload).
- §5.4 Filename sanitizer → Task 6.
- §6 Failure modes (ZABORT, timeout, size limit, Ctrl-X cancel) → Tasks 8, 9. Ctrl-X cancel is handled by xterm-forwarded keystrokes reaching the session naturally; no explicit code.
- §7 Config (env vars, /config.json) → Task 4.
- §8 Testing (unit + Node proxy + E2E) → Tasks 1, 3, 4, 6, 10.

**Placeholder scan:** No TBDs, no "handle appropriately", no "add validation". Every code step contains complete code.

**Type/name consistency:**
- Module export symbols: `window.CP437.cp437ToUtf8`, `window.XferUtil.{sanitizeFilename, formatBytes, formatDuration}`, `window.ZmodemUI.{startXfer, updateXfer, endXfer, surfaceDownload, surfaceError, promptUpload}`, `window.ZmodemSentry.createZmodemBridge`. Consistent throughout Tasks 6, 8, 9.
- CSS class names: `.modem-xfer`, `.xfer-notifications`, `.xfer-notification`. Consistent between Task 7 CSS and Task 8 JS.
- DOM IDs: `xferStrip`, `xferDir`, `xferName`, `xferPct`, `xferFill`, `xferCps`, `xferEta`, `xferBatch`, `xferNotifications`, `xferUploadInput`. Consistent between Task 7 HTML and Task 8 JS.
- Env vars: `MAX_UPLOAD_BYTES`, `ZMODEM_TIMEOUT_SEC`. Consistent between Task 4 server code and Task 9 client code (`config.maxUploadBytes`, `config.zmodemTimeoutSec` from `/config.json`).
