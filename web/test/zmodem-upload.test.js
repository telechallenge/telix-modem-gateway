// Regression tests for the ZMODEM upload path. Both bugs covered here left
// `rz` waiting forever, so they are driven through the real zmodem.js rather
// than asserted against our own abstractions.

const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');
const vm = require('node:vm');

// The browser modules are plain scripts that talk to each other through
// globals, so give them a real global object rather than a function scope.
function loadSandbox() {
  const g = {
    console: { log() {}, warn() {}, error() {}, debug() {} },
    setTimeout, clearTimeout, performance,
    Uint8Array, TextDecoder,
    Blob: class { constructor(c) { this.chunks = c; } },
    WebSocket: { OPEN: 1 },
  };
  g.window = g;
  vm.createContext(g);
  for (const f of ['cp437.js', 'xfer-util.js', 'vendor/zmodem.min.js', 'zmodem-sentry.js']) {
    const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', f), 'utf8');
    vm.runInContext(src, g, { filename: f });
  }
  return g;
}

// A ZRINIT is what `rz` sends to say "send me a file". lrzsz repeats it every
// ~10s until it gets a ZFILE.
function zrinit(g) {
  // ESCCTL keeps the sender from detouring through ZSINIT first, so the ZFILE
  // goes out on the next tick and the test can look for it.
  const hex = g.Zmodem.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'ESCCTL']).to_hex();
  return Uint8Array.from(hex);
}

function makeBridge(g, { files, onSend }) {
  let resolvePicker;
  const picked = new Promise(res => { resolvePicker = res; });
  const events = [];

  const bridge = g.window.ZmodemSentry.createZmodemBridge({
    ws: { readyState: 1, send: b => onSend(Buffer.from(b)) },
    term: { write() {} },
    config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30 },
    checkModemState() {},
    flashLed() {},
  });

  g.window.ZmodemUI = {
    startXfer: d => events.push(['startXfer', d.filename]),
    updateXfer() {},
    endXfer: d => events.push(['endXfer', d.status]),
    surfaceError: m => events.push(['error', String(m)]),
    surfaceDownload() {},
    promptUpload: () => picked,
  };

  return { bridge, events, offer: () => resolvePicker(files) };
}

function fakeFile(name, bytes) {
  const buf = Uint8Array.from(bytes);
  return {
    name, size: buf.length, lastModified: 1700000000000,
    arrayBuffer: async () => buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.length),
  };
}

test('repeated ZRINIT while the file picker is open does not kill the session', async () => {
  const g = loadSandbox();
  const sent = [];
  const { bridge, events, offer } = makeBridge(g, {
    files: [fakeFile('hello.bin', [1, 2, 3, 4])],
    onSend: b => sent.push(b),
  });

  const hdr = zrinit(g);
  // The picker is still open across all of these — exactly what happens while
  // the user is choosing a file. Previously the second one threw
  // "Cannot read properties of null (reading 'ZRINIT')" and the upload died.
  assert.doesNotThrow(() => {
    for (let i = 0; i < 4; i++) bridge.consume(hdr);
  });

  offer();
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));

  assert.ok(
    events.some(e => e[0] === 'startXfer'),
    `expected the upload to start, got ${JSON.stringify(events)}`
  );
  assert.ok(
    !events.some(e => e[0] === 'endXfer' && e[1] === 'aborted'),
    `upload aborted: ${JSON.stringify(events)}`
  );
});

test('single-file upload sends an offer instead of failing validation', async () => {
  const g = loadSandbox();
  const sent = [];
  const { bridge, events, offer } = makeBridge(g, {
    files: [fakeFile('hello.bin', [9, 8, 7])],
    onSend: b => sent.push(b),
  });

  bridge.consume(zrinit(g));
  offer();
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));

  // files_remaining is 0 for the only file in a batch, and zmodem.js rejects a
  // literal 0 — it has to be omitted. Passing it aborted every single-file
  // upload before any data went out.
  assert.ok(
    !events.some(e => e[0] === 'error' || (e[0] === 'endXfer' && e[1] === 'aborted')),
    `offer was rejected: ${JSON.stringify(events)}`
  );
  const wire = Buffer.concat(sent);
  assert.ok(wire.length > 0, 'expected a ZFILE offer on the wire');
  assert.ok(wire.includes(Buffer.from('hello.bin')), 'ZFILE should carry the filename');
});

test('cancelling the picker denies the session and sends nothing', async () => {
  const g = loadSandbox();
  const sent = [];
  const { bridge, offer } = makeBridge(g, { files: [], onSend: b => sent.push(b) });

  bridge.consume(zrinit(g));
  offer();
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));

  const wire = Buffer.concat(sent);
  assert.ok(!wire.includes(Buffer.from('hello.bin')), 'no file should be offered');
});

test('keystrokes stay blocked while the picker is open', () => {
  const g = loadSandbox();
  const { bridge } = makeBridge(g, { files: [], onSend() {} });

  assert.strictEqual(bridge.isActive(), false, 'idle before any ZMODEM traffic');
  bridge.consume(zrinit(g));
  assert.strictEqual(bridge.isActive(), true, 'active once an upload is pending');
});

// Multi-file download where the sender opens a fresh ZMODEM session per file and
// never sends ZFIN between them (DOS DSZ/GSZ behind a telnet bridge). The
// previous receive session is still live when the next file's ZRQINIT lands, so
// dropping every ZRQINIT-during-session left only the first file downloaded.
// Driven with the real zmodem.js library on both ends so it exercises the true
// protocol, not our abstractions.
test('per-file sessions without ZFIN still deliver every file', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;
  const tick = () => new Promise(r => setImmediate(r));

  const received = [];
  g.window.ZmodemUI = {
    startXfer() {}, updateXfer() {}, endXfer() {},
    surfaceError() {}, promptUpload: () => Promise.resolve([]),
    surfaceDownload: d => received.push({
      name: d.filename,
      buf: Buffer.concat(d.blob.chunks.map(u => Buffer.from(u))),
    }),
  };

  let sendSession = null;
  const bridge = g.window.ZmodemSentry.createZmodemBridge({
    ws: { readyState: 1, send: b => {
      const arr = Array.from(b);
      if (!sendSession) {
        // The bridge's ZRINIT reply is what constructs the sender.
        try {
          const s = Z.Session.parse(arr.slice());
          if (s && s.type === 'send') { sendSession = s; sendSession.set_sender(o => toBridge(o)); }
        } catch (_) { /* not a full header yet */ }
        return;
      }
      try { sendSession.consume(arr); } catch (_) { /* ignore */ }
    } },
    term: { write() {} },
    config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30 },
    checkModemState() {}, flashLed() {},
  });
  const toBridge = octets => bridge.consume(Uint8Array.from(octets));

  const files = [
    { name: 'PKZ199B.EXE', data: Buffer.from(Array.from({ length: 1200 }, (_, i) => (i * 7 + 1) & 0xff)) },
    { name: 'PKZBUG2.TXT', data: Buffer.from(Array.from({ length: 900 }, (_, i) => (i * 13 + 5) & 0xff)) },
    { name: 'PROBDESC.ARJ', data: Buffer.from(Array.from({ length: 1500 }, (_, i) => (i * 3 + 9) & 0xff)) },
  ];

  for (const f of files) {
    sendSession = null;                         // each file is a separate session
    toBridge(Array.from(Buffer.from('rz\r', 'latin1')));
    toBridge(Z.Header.build('ZRQINIT').to_hex()); // bridge replies ZRINIT -> builds sender
    for (let i = 0; i < 5 && !sendSession; i++) await tick();
    assert.ok(sendSession, `no send session formed for ${f.name}`);
    const xfer = await sendSession.send_offer({ name: f.name, size: f.data.length });
    assert.ok(xfer, `offer skipped for ${f.name}`);
    await xfer.send(Array.from(f.data));
    await xfer.end();
    // Deliberately NO sendSession.close(): the DSZ shape this test guards.
    await tick();
  }
  await new Promise(r => setTimeout(r, 30));

  assert.strictEqual(received.length, files.length,
    `expected all ${files.length} files, got ${received.map(r => r.name).join(', ') || 'none'}`);
  for (const f of files) {
    const got = received.find(r => r.name === f.name);
    assert.ok(got && got.buf.equals(f.data), `${f.name} missing or corrupted`);
  }
});

// DSZ/GSZ (and most DOS ZMODEM) advertise a non-zero receive buffer in ZRINIT.
// zmodem.js only supports streaming receivers (buffer 0) and throws
// "Buffer size unsupported" on the rest — swallowed by the Sentry, so the
// upload never even opens the file picker. The bridge rewrites the ZRINIT to
// advertise buffer 0 (the sender streams; the receiver copes over TCP).
test('a ZRINIT advertising a non-zero buffer still triggers the upload picker', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;

  let promptCalls = 0;
  g.window.ZmodemUI = {
    startXfer() {}, updateXfer() {}, endXfer() {}, surfaceError() {}, surfaceDownload() {},
    promptUpload: () => { promptCalls++; return new Promise(() => {}); }, // user hasn't picked yet
  };
  const bridge = g.window.ZmodemSentry.createZmodemBridge({
    ws: { readyState: 1, send() {} }, term: { write() {} },
    config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30 },
    checkModemState() {}, flashLed() {},
  });

  // Build a ZRINIT the way DSZ would: capability flags plus a non-zero buffer.
  const zrinitStreaming = Buffer.from(Z.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex());
  // Craft a buffer-bearing one by hand (type 01, ZP0=0x00 ZP1=0x20, flags 0x23)
  // with a correct CRC, mirroring the real BBS header.
  const data = [0x01, 0x00, 0x20, 0x00, 0x23];
  const crc = Z.CRC.crc16(data);
  const hex = Z.ENCODELIB.octets_to_hex(data.concat(crc)); // array of ascii byte values
  const withBuffer = Uint8Array.from([0x2a, 0x2a, 0x18, 0x42, ...hex, 0x0d, 0x0a, 0x11]);

  // Sanity: this header alone makes a raw send session throw.
  assert.throws(() => Z.Session.parse(Array.from(withBuffer)), /Buffer size/);

  // Through the bridge it must be accepted and open the picker.
  bridge.consume(withBuffer);
  assert.strictEqual(promptCalls, 1, 'buffer-bearing ZRINIT should open the picker exactly once');

  // And a plain streaming ZRINIT still works (regression guard).
  const g2 = loadSandbox();
  let prompt2 = 0;
  g2.window.ZmodemUI = { startXfer() {}, updateXfer() {}, endXfer() {}, surfaceError() {}, surfaceDownload() {}, promptUpload: () => { prompt2++; return new Promise(() => {}); } };
  const b2 = g2.window.ZmodemSentry.createZmodemBridge({ ws: { readyState: 1, send() {} }, term: { write() {} }, config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30 }, checkModemState() {}, flashLed() {} });
  b2.consume(zrinitStreaming);
  assert.strictEqual(prompt2, 1, 'streaming ZRINIT should still open the picker');
});
