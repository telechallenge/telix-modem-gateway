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

function makeBridge(g, { files, onSend, ws, config, term }) {
  let resolvePicker;
  const picked = new Promise(res => { resolvePicker = res; });
  const events = [];

  const bridge = g.window.ZmodemSentry.createZmodemBridge({
    ws: ws || { readyState: 1, send: b => onSend(Buffer.from(b)) },
    term: term || { write() {} },
    config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30, ...config },
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

// Builds a ZRINIT advertising `bufsize` bytes of receive window, the way
// Searchlight BBS and DOS ZMODEM generally do. ZP0 is the low byte, ZP1 the
// high byte; flags 0x23 is CANFDX|CANOVIO|CANFC32.
function zrinitWithWindow(g, bufsize) {
  const data = [0x01, bufsize & 0xff, (bufsize >> 8) & 0xff, 0x00, 0x23];
  const hex = g.Zmodem.ENCODELIB.octets_to_hex(data.concat(g.Zmodem.CRC.crc16(data)));
  return Uint8Array.from([0x2a, 0x2a, 0x18, 0x42, ...hex, 0x0d, 0x0a, 0x11]);
}

const wireBytes = sent => sent.reduce((n, b) => n + b.length, 0);

// Resolves once the sender has stopped emitting — i.e. it is blocked waiting on
// us. That pause is the whole behaviour under test, so it is the signal we key
// on rather than a fixed sleep.
async function quiesce(sent) {
  let last = -1;
  for (let i = 0; i < 200 && wireBytes(sent) !== last; i++) {
    last = wireBytes(sent);
    await new Promise(r => setTimeout(r, 10));
  }
  return wireBytes(sent);
}

// A receiver that advertises a window expects the sender to stop every
// `bufsize` bytes and wait for a ZACK. zmodem.js cannot do that — it streams
// ZCRCG subpackets until the file runs out — and because we rewrite the ZRINIT
// to get past its "Buffer size is unsupported" check, nothing else paces the
// send either. Searchlight BBS answers the resulting burst with CAN*8 and drops
// carrier, while the browser still reports 100% because progress counts bytes
// handed to zmodem.js rather than bytes acknowledged.
test('a windowed receiver is paced to its window instead of firehosed', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;
  const WINDOW = 8192;
  const SIZE = WINDOW * 5;

  const sent = [];
  // Printable bytes keep ZDLE escaping out of the wire-size arithmetic below.
  const payload = Array.from({ length: SIZE }, (_, i) => 0x41 + (i % 26));
  const { bridge, events, offer } = makeBridge(g, {
    files: [fakeFile('BANNER.ANS', payload)],
    onSend: b => sent.push(b),
  });
  const respond = octets => bridge.consume(Uint8Array.from(octets));

  bridge.consume(zrinitWithWindow(g, WINDOW));
  offer();

  // The receiver did not offer ESCCTL, so zmodem.js detours through a ZSINIT
  // and waits for a ZACK before it will send the offer.
  await quiesce(sent);
  respond(Z.Header.build('ZACK').to_hex());
  await quiesce(sent);
  const beforeData = wireBytes(sent);
  respond(Z.Header.build('ZRPOS', 0).to_hex());

  // First window: the sender must stop here, not run to the end of the file.
  let prev = beforeData;
  const bursts = [(await quiesce(sent)) - prev];
  assert.ok(
    bursts[0] < SIZE,
    `sender firehosed the whole ${SIZE}-byte file without waiting for a ZACK`
  );

  // Release the remaining windows one ZACK at a time. The last ZACK frees the
  // sender to emit its closing ZDATA/ZEOF, so there is one round per window.
  for (let i = 0; i < SIZE / WINDOW; i++) {
    prev = wireBytes(sent);
    respond(Z.Header.build('ZACK').to_hex());
    bursts.push((await quiesce(sent)) - prev);
  }

  // Every data burst must sit at the window, not just the first — a sender that
  // paced once and then opened up would still drown the receiver.
  const dataBursts = bursts.filter(n => n > WINDOW / 2);
  assert.strictEqual(
    dataBursts.length, SIZE / WINDOW,
    `expected ${SIZE / WINDOW} windows of ~${WINDOW} bytes, got ${JSON.stringify(bursts)}`
  );
  for (const n of dataBursts) {
    assert.ok(
      n < WINDOW * 1.5,
      `a burst of ${n} bytes overran the ${WINDOW}-byte window: ${JSON.stringify(bursts)}`
    );
  }

  // ZEOF waits on a ZRINIT, then close() sends ZFIN and waits for the peer's.
  respond(Z.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex());
  await quiesce(sent);
  respond(Z.Header.build('ZFIN').to_hex());
  await quiesce(sent);

  assert.ok(
    !events.some(e => e[0] === 'error' || (e[0] === 'endXfer' && e[1] === 'aborted')),
    `transfer did not survive windowed pacing: ${JSON.stringify(events)}`
  );
  assert.ok(
    events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `expected the upload to finish, got ${JSON.stringify(events)}`
  );
  assert.ok(
    wireBytes(sent) >= SIZE,
    `only ${wireBytes(sent)} bytes reached the wire for a ${SIZE}-byte file`
  );
});

// Pacing alone still left a hole, and it is the one the real BBS fell into: a
// file *smaller* than the advertised window never fills it, so the loop above
// never closes a frame and the whole file goes out unacknowledged — every
// subpacket ZCRCG, the closing one ZCRCE, both "no ack". The sender's only
// evidence of delivery was then the ZRINIT it waits for after ZEOF, and that is
// byte-identical to the ZRINIT a receiver re-announces when it has given up. So
// a 4279-byte upload that landed nowhere reported "receiver acknowledged the
// file / session closed cleanly" with the browser sitting at 100%.
const SHORT_FILE = 4279; // the size from the failing Searchlight session

function shortFileBridge(g) {
  const sent = [];
  const made = makeBridge(g, {
    files: [fakeFile('BORING.ANS', Array.from({ length: SHORT_FILE }, (_, i) => 0x41 + (i % 26)))],
    onSend: b => sent.push(b),
  });
  return { ...made, sent, respond: octets => made.bridge.consume(Uint8Array.from(octets)) };
}

// Drives the exchange to the point where the whole short file has been sent.
async function sendShortFile(g, h) {
  h.bridge.consume(zrinitWithWindow(g, 8192));
  h.offer();
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZACK').to_hex()); // acknowledges our ZSINIT
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZRPOS', 0).to_hex());
  await quiesce(h.sent);
}

test('a short file is not reported as sent when the receiver never acknowledges it', async () => {
  const g = loadSandbox();
  const h = shortFileBridge(g);
  await sendShortFile(g, h);

  // The receiver acknowledges nothing and simply repeats the ZRINIT it opened
  // with — the exact shape of the failing BBS session.
  h.respond(zrinitWithWindow(g, 8192));
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZFIN').to_hex());
  await quiesce(h.sent);

  assert.ok(
    !h.events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `an unacknowledged upload reported success: ${JSON.stringify(h.events)}`
  );
});

// send_offer resolves undefined for exactly one reason — the receiver answered
// the ZFILE with ZSKIP, i.e. refused the file. Reporting that as 'done' claimed
// an upload had succeeded when nothing was sent at all; Searchlight skips a
// filename it already holds, so this is what a repeat attempt looks like.
test('a refused offer (ZSKIP) is not reported as a completed upload', async () => {
  const g = loadSandbox();
  const h = shortFileBridge(g);

  h.bridge.consume(zrinitWithWindow(g, 8192));
  h.offer();
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZACK').to_hex()); // acknowledges our ZSINIT
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZSKIP').to_hex());
  await quiesce(h.sent);

  assert.ok(
    !h.events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `a refused file reported success: ${JSON.stringify(h.events)}`
  );
  assert.ok(
    h.events.some(e => e[0] === 'endXfer' && e[1] === 'skipped'),
    `expected the refusal to be surfaced, got ${JSON.stringify(h.events)}`
  );
});

test('a short file completes once the receiver acknowledges the closing frame', async () => {
  const g = loadSandbox();
  const h = shortFileBridge(g);
  await sendShortFile(g, h);

  h.respond(g.Zmodem.Header.build('ZACK').to_hex()); // closes the ZCRCW frame
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex());
  await quiesce(h.sent);
  h.respond(g.Zmodem.Header.build('ZFIN').to_hex());
  await quiesce(h.sent);

  assert.ok(
    h.events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `acknowledged upload did not finish: ${JSON.stringify(h.events)}`
  );
});

// The contrast case: a receiver advertising buffer 0 really does want the
// firehose, and must not be slowed to a ZACK round-trip per window.
test('a streaming receiver is not paced', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;
  const SIZE = 8192 * 3;

  const sent = [];
  const { bridge, offer } = makeBridge(g, {
    files: [fakeFile('BANNER.ANS', Array.from({ length: SIZE }, (_, i) => 0x41 + (i % 26)))],
    onSend: b => sent.push(b),
  });
  const respond = octets => bridge.consume(Uint8Array.from(octets));

  bridge.consume(zrinitWithWindow(g, 0));
  offer();
  await quiesce(sent);
  respond(Z.Header.build('ZACK').to_hex());
  await quiesce(sent);
  const beforeData = wireBytes(sent);
  respond(Z.Header.build('ZRPOS', 0).to_hex());

  const emitted = (await quiesce(sent)) - beforeData;
  assert.ok(
    emitted >= SIZE,
    `streaming receiver should get all ${SIZE} bytes without a ZACK, got ${emitted}`
  );
});

// Uploads a single-byte-payload file and returns the bytes that carried it. The
// payload byte is one ZMODEM never escapes and one that never appears in a
// header, so the longest run of it on the wire is exactly the largest data
// subpacket — no framing arithmetic to get wrong.
async function measureSubpacket(g, { window, size, blockSize }) {
  const Z = g.Zmodem;
  const sent = [];
  const { bridge, offer } = makeBridge(g, {
    files: [fakeFile('BANNER.ANS', new Array(size).fill(0x41))],
    onSend: b => sent.push(b),
    config: blockSize ? { zmodemBlockSize: blockSize } : undefined,
  });
  const respond = octets => bridge.consume(Uint8Array.from(octets));

  bridge.consume(zrinitWithWindow(g, window));
  offer();
  await quiesce(sent);
  respond(Z.Header.build('ZACK').to_hex()); // acknowledges our ZSINIT
  await quiesce(sent);
  const start = wireBytes(sent);
  respond(Z.Header.build('ZRPOS', 0).to_hex());
  await quiesce(sent);
  // A windowed receiver parks the sender after every window until it acks.
  for (let i = 0; window > 0 && i <= Math.ceil(size / window); i++) {
    respond(Z.Header.build('ZACK').to_hex());
    await quiesce(sent);
  }

  const wire = Buffer.concat(sent).subarray(start);
  let longest = 0;
  let run = 0;
  for (const b of wire) {
    if (b === 0x41) { run++; if (run > longest) longest = run; }
    else run = 0;
  }
  assert.ok(longest > 0, 'expected file data on the wire');
  return longest;
}

// The sender picks the data subpacket size; ZMODEM negotiates nothing about it,
// which is why "ZMODEM-8k" is a name on a BBS protocol menu rather than a flag
// in the handshake. Both sizes here are load-bearing and were learned the hard
// way against real boards:
//
//   - Searchlight BBS 5.1 (which advertises a receive window) answers anything
//     over 1 KiB with `ZRPOS 0` — "start over" — and loops there until the
//     transfer is cancelled, so the file never lands. 4 KB slices failed every
//     time against it; 1 KiB slices are accepted.
//   - ENiGMA½ receives with `sexyz -telnet -8 rz` or lrzsz's `rz` (its two
//     default protocols are literally named "ZModem 8k (SEXYZ)" and "ZModem
//     8k"). Both stream — buffer 0 in their ZRINIT — and both assemble
//     subpackets into an 8192-byte buffer.
test('auto sends the 1 KiB the specification names, whatever the ZRINIT says', async () => {
  // This used to infer 8 KiB from a receiver advertising no window. MajorBBS
  // advertises buffer 0, refuses 8 KiB with `ZRPOS 0`, and accepts the identical
  // file once the fallback drops to 1 KiB — so the inference was wrong, and the
  // trade is lopsided: guessing 8 KiB on a board that refuses it costs a full
  // re-send of everything already in flight, guessing 1 KiB on a board that
  // would have taken 8 KiB costs ~15% throughput.
  assert.strictEqual(
    await measureSubpacket(loadSandbox(), { window: 0, size: 16384 }), 1024,
    'a streaming receiver must not be assumed to take 8k — MajorBBS does not'
  );
  assert.strictEqual(
    await measureSubpacket(loadSandbox(), { window: 8192, size: 16384 }), 1024,
    'a windowed receiver is the DOS-era population Searchlight belongs to; it must keep 1 KiB'
  );
  assert.strictEqual(
    await measureSubpacket(loadSandbox(), { window: 0, size: 16384, blockSize: 8192 }), 8192,
    '8k stays available, but only when it is asked for'
  );
});

// The auto rule above is an inference from two measured populations, not a
// guarantee the protocol makes, so a board that disagrees has to be pinnable
// without a code change.
// SEXYZ — ENiGMA½'s default "ZModem 8k (SEXYZ)" receiver — answers our ZSINIT
// with a ZACK and then loops straight back to re-announcing itself, so the ZACK
// and the next ZRINIT arrive in one TCP segment. zmodem.js clears
// `_next_header_handler` the moment it dispatches a header and reinstalls the
// next one on a microtask, while our splitter feeds every header of a chunk in
// the same synchronous turn — so the ZRINIT landed in that null window and
// `_consume_header` threw "Cannot read properties of null (reading 'ZRINIT')".
// The bridge tore the session down, yet zmodem.js's own promise chain carried on
// and sent the ZFILE regardless, which is why the receiver's ZRPOS then printed
// as line noise on a dead session.
test('a ZRINIT coalesced behind the ZSINIT ack does not kill a SEXYZ upload', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;
  const sent = [];
  const { bridge, events, offer } = makeBridge(g, {
    files: [fakeFile('SIMCITY2.ZIP', new Uint8Array(4096).fill(0x41))],
    onSend: b => sent.push(b),
  });

  // flags 0x23 (no ESCCTL) is what SEXYZ and lrzsz both advertise, and it is
  // what makes zmodem.js detour through a ZSINIT before the offer.
  bridge.consume(zrinitWithWindow(g, 0));
  offer();
  await quiesce(sent);

  // The segment that used to be fatal: ZACK for our ZSINIT, then the receiver's
  // next keep-alive ZRINIT, delivered together.
  bridge.consume(Uint8Array.from([
    ...Z.Header.build('ZACK').to_hex(),
    ...zrinitWithWindow(g, 0),
  ]));
  await quiesce(sent);

  assert.ok(
    Buffer.concat(sent).includes(Buffer.from('SIMCITY2.ZIP')),
    `the ZFILE offer never reached the wire: ${JSON.stringify(events)}`
  );
  assert.ok(
    !events.some(e => e[0] === 'endXfer' && e[1] === 'aborted'),
    `the keep-alive ZRINIT aborted the session: ${JSON.stringify(events)}`
  );

  // And the session is still healthy enough to finish the file.
  bridge.consume(Uint8Array.from(Z.Header.build('ZRPOS', 0).to_hex()));
  await quiesce(sent);
  bridge.consume(Uint8Array.from(Z.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex()));
  await quiesce(sent);
  assert.ok(
    events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `upload did not complete after the coalesced ZRINIT: ${JSON.stringify(events)}`
  );
});

// The timeout path used to end a transfer in complete silence, which is why
// every failing session anyone pasted stopped mid-sentence — indistinguishable
// from the BBS hanging up.
test('a timeout says so on the terminal instead of ending in silence', async () => {
  const g = loadSandbox();
  const lines = [];
  const { bridge, events, offer } = makeBridge(g, {
    files: [fakeFile('BORING.ANS', new Uint8Array(64).fill(0x41))],
    onSend() {},
    term: { write: s => lines.push(s) },
    config: { zmodemTimeoutSec: 0.1 },
  });

  bridge.consume(zrinitWithWindow(g, 0));
  offer();
  await new Promise(r => setTimeout(r, 400)); // never answer the ZSINIT

  assert.ok(
    events.some(e => e[0] === 'endXfer' && e[1] === 'timeout'),
    `expected a timeout, got ${JSON.stringify(events)}`
  );
  assert.ok(
    lines.some(l => l.includes('TIMED OUT')),
    `the timeout left no trace on the terminal: ${JSON.stringify(lines)}`
  );
});

// A browser WebSocket whose send() queues rather than blocks, exactly like the
// real one: bytes pile up in `bufferedAmount` and drain on the network's
// schedule, not the sender's.
function drainingWS({ perTick, tickMs }) {
  const ws = {
    readyState: 1,
    bufferedAmount: 0,
    peak: 0,
    total: 0,
    head: Buffer.alloc(0), // first few KB, so a test can spot the ZFILE offer
    send(b) {
      if (ws.head.length < 4096) ws.head = Buffer.concat([ws.head, Buffer.from(b)]);
      ws.bufferedAmount += b.length;
      ws.total += b.length;
      if (ws.bufferedAmount > ws.peak) ws.peak = ws.bufferedAmount;
    },
  };
  ws.timer = setInterval(() => {
    ws.bufferedAmount = Math.max(0, ws.bufferedAmount - perTick);
  }, tickMs);
  return ws;
}

async function waitFor(pred, ms) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return true;
    await new Promise(r => setTimeout(r, 10));
  }
  return false;
}

// ws.send() appends to a buffer and returns; nothing above it blocked either, so
// the send loop used to hand zmodem.js the whole file as fast as JavaScript
// could escape it. The browser then showed 100%, the loop stopped re-arming the
// idle timer, and 30s later the session was aborted — silently, because the
// timeout path does not trace — while the first megabyte was still on the wire.
// That is how a 116 MB upload to ENiGMA½ ended at "all 121779977 bytes sent,
// waiting for the receiver" and nothing after it.
//
// The timeout here (250ms) is far shorter than the ~600ms this file takes to
// drain, so a sender that does not re-arm on real progress cannot pass, and the
// peak assertion fails for any sender that queues faster than the socket drains.
test('a large upload paces itself to the socket and is not timed out by its own backlog', async () => {
  const g = loadSandbox();
  const Z = g.Zmodem;
  const SIZE = 4 << 20;
  const ws = drainingWS({ perTick: 128 * 1024, tickMs: 20 });
  const lines = [];

  try {
    const { bridge, events, offer } = makeBridge(g, {
      files: [fakeFile('SIMCITY2.ZIP', new Uint8Array(SIZE).fill(0x41))],
      ws,
      term: { write: s => lines.push(s) },
      config: { zmodemTimeoutSec: 0.25 },
    });
    const respond = octets => bridge.consume(Uint8Array.from(octets));

    bridge.consume(zrinit(g)); // ESCCTL, so no ZSINIT detour before the offer
    offer();
    assert.ok(
      await waitFor(() => ws.head.includes(Buffer.from('SIMCITY2.ZIP')), 2000),
      'the ZFILE offer never reached the wire'
    );
    respond(Z.Header.build('ZRPOS', 0).to_hex());

    assert.ok(
      await waitFor(() => lines.some(l => l.includes('for the receiver')), 20000),
      `the data phase never finished (${ws.total} of ${SIZE} bytes queued)`
    );
    respond(Z.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex());
    await waitFor(() => events.some(e => e[0] === 'endXfer'), 3000);
    respond(Z.Header.build('ZFIN').to_hex()); // let close() finish

    assert.deepStrictEqual(
      events.filter(e => e[0] === 'endXfer'), [['endXfer', 'done']],
      `a transfer that was progressing the whole time did not complete: ${JSON.stringify(events)}`
    );
    assert.ok(
      ws.peak < 2 << 20,
      `peaked at ${ws.peak} bytes queued for a ${SIZE}-byte file; the sender is ` +
      'not waiting for the socket, so progress and the idle timer both measure the loop'
    );
  } finally {
    clearInterval(ws.timer);
  }
});

test('chooseBlockSize ignores a size that is not one of the two real ones', () => {
  const { chooseBlockSize } = loadSandbox().window.ZmodemSentry;
  // Anything else on the wire is neither the spec size nor the 8k variant, so
  // it falls back to the auto rule rather than being honoured.
  assert.strictEqual(chooseBlockSize(0, 4096), 1024);
  assert.strictEqual(chooseBlockSize(8192, 4096), 1024);
  assert.strictEqual(chooseBlockSize(0, 0), 1024);
  assert.strictEqual(chooseBlockSize(0, 8192), 8192, '8k is still reachable by asking');
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

// zmodem.js's streaming sender registers no header handler for the whole data
// phase, and `_consume_header` reads `_next_header_handler[NAME]` straight off a
// null — so *any* header the receiver sent mid-stream killed the upload with
// "Cannot read properties of null (reading 'ZRPOS')". SEXYZ does send one.
// ZRPOS is the receiver asking us to resume from an offset, which is ZMODEM's
// own repair mechanism and something we can honour: the file is already in
// memory, so there is nothing to re-read.
async function uploadWithRewind(g, { rewindsToSend, target, size }) {
  const Z = g.Zmodem;
  const sent = [];
  const lines = [];
  let inData = false;
  let rewindsSent = 0;
  let bridge;

  // The send loop is an unbroken microtask chain, so a timer would not fire
  // until it had finished — the header has to be injected from the write that
  // provokes it, which is also exactly how a fast link delivers one.
  const ws = {
    readyState: 1,
    send(b) {
      sent.push(Buffer.from(b));
      if (!inData || rewindsSent >= rewindsToSend) return;
      if (wireBytes(sent) < 9000 * (rewindsSent + 1)) return;
      rewindsSent++;
      bridge.consume(Uint8Array.from(Z.Header.build('ZRPOS', target).to_hex()));
    },
  };

  const made = makeBridge(g, {
    files: [fakeFile('SIMCITY2.ZIP', new Uint8Array(size).fill(0x41))],
    ws,
    term: { write: s => lines.push(s) },
    config: { zmodemBlockSize: 8192 },
  });
  bridge = made.bridge;
  const respond = o => bridge.consume(Uint8Array.from(o));

  bridge.consume(zrinitWithWindow(g, 0));
  made.offer();
  await quiesce(sent);
  respond(Z.Header.build('ZACK').to_hex());     // acknowledges our ZSINIT
  await quiesce(sent);
  inData = true;
  respond(Z.Header.build('ZRPOS', 0).to_hex()); // accepts the offer
  await quiesce(sent);

  return { ...made, sent, lines, respond, payloadBytes: () => {
    let n = 0;
    for (const b of Buffer.concat(sent)) if (b === 0x41) n++;
    return n;
  } };
}

test('a mid-stream ZRPOS rewinds the send and drops to 1 KiB subpackets', async () => {
  const g = loadSandbox();
  const SIZE = 32768;
  const TARGET = 4096;
  const h = await uploadWithRewind(g, { rewindsToSend: 1, target: TARGET, size: SIZE });

  // The header trace is what makes a pasted session diagnosable: a ZRPOS
  // answering the ZFILE is the offer being accepted, the same header later means
  // the receiver lost sync, and only the order tells them apart.
  assert.ok(
    h.lines.some(l => l.includes('<- ZRPOS offset=0')),
    `the offer-accepting ZRPOS was not named: ${JSON.stringify(h.lines.map(l => l.trim()).filter(Boolean))}`
  );
  assert.ok(
    h.lines.some(l => l.includes(`<- ZRPOS offset=${TARGET}`)),
    'the mid-stream rewind request must be named with its offset'
  );
  assert.ok(
    h.lines.some(l => l.includes(`rewound to offset ${TARGET}`)),
    `the rewind was not honoured: ${JSON.stringify(h.lines.map(l => l.trim()).filter(Boolean))}`
  );
  assert.ok(
    h.lines.some(l => l.includes('1024-byte subpackets')),
    'the first rewind should fall back to the 1 KiB the specification names'
  );
  assert.ok(
    h.payloadBytes() > SIZE,
    `nothing was resent (${h.payloadBytes()} payload bytes for a ${SIZE}-byte file)`
  );

  h.respond(g.Zmodem.Header.build('ZRINIT', ['CANFDX', 'CANOVIO', 'CANFC32']).to_hex());
  await quiesce(h.sent);
  assert.ok(
    h.events.some(e => e[0] === 'endXfer' && e[1] === 'done'),
    `the upload did not recover: ${JSON.stringify(h.events)}`
  );
});

// Resending is only worth it if it can converge, and each round is a full
// re-upload — so it has to stop, and the reason it gives has to be the right one.
// A receiver stuck at offset 0 has never taken a single data byte, which rules
// out the block size (the ZFILE offer got through, and that is plain ASCII);
// only binary data is being rejected, so something is altering high bytes. One
// retry is enough to learn that, and on a 116 MB upload the difference between
// one wasted re-send and three is the whole afternoon.
test('a receiver stuck at offset 0 is diagnosed, not retried into the ground', async () => {
  const g = loadSandbox();
  const h = await uploadWithRewind(g, { rewindsToSend: 99, target: 0, size: 32768 });

  assert.ok(
    h.events.some(e => e[0] === 'endXfer' && e[1] === 'aborted'),
    `expected the transfer to give up, got ${JSON.stringify(h.events)}`
  );
  const why = h.lines.find(l => l.includes('FAILED')) || '';
  assert.match(why, /rewound us 2 times/, 'nothing was ever accepted, so one retry is the budget');
  assert.match(why, /never accepted a single data byte/);
  // A board refusing the file on size answers ZRPOS 0 and keeps answering it,
  // which is indistinguishable from a corrupt link — and it is far commoner, so
  // it has to be named first, with the size to check the limit against.
  assert.match(why, /refusing the file on size/);
  assert.match(why, /32768 bytes/, 'the message must carry the size to compare with the limit');
  assert.match(why, /0xFF/, 'the escaping hypothesis stays, as the second suspect');
  assert.match(why, /outbound_iac/, 'it must name where that verdict is recorded');
});

// A receiver that *is* taking data and rewinds part-way through is the ordinary
// line-noise case, and that one is worth a few more attempts.
test('a rewind part-way through the file is retried rather than diagnosed', async () => {
  const g = loadSandbox();
  const h = await uploadWithRewind(g, { rewindsToSend: 99, target: 8192, size: 32768 });

  const why = h.lines.find(l => l.includes('FAILED')) || '';
  assert.match(why, /rewound us 4 times/, 'a receiver making progress gets the full budget');
  assert.match(why, /not arriving intact/);
});

// ZMODEM_BLOCK_SIZE is a deployment-wide override for the auto rule. There is no
// per-upload picker: the size cannot be detected, and 1 KiB needs no choosing.
test('a configured block size overrides what the ZRINIT implies', async () => {
  const g = loadSandbox();
  const sent = [];
  const { bridge, offer } = makeBridge(g, {
    files: [fakeFile('BANNER.ANS', new Uint8Array(8192).fill(0x41))],
    onSend: b => sent.push(b),
    config: { zmodemBlockSize: 1024 },
  });
  const respond = o => bridge.consume(Uint8Array.from(o));

  bridge.consume(zrinitWithWindow(g, 0)); // streaming: auto would say 8192
  offer();
  await quiesce(sent);
  respond(g.Zmodem.Header.build('ZACK').to_hex());
  await quiesce(sent);
  const start = wireBytes(sent);
  respond(g.Zmodem.Header.build('ZRPOS', 0).to_hex());
  await quiesce(sent);

  let longest = 0, run = 0;
  for (const b of Buffer.concat(sent).subarray(start)) {
    if (b === 0x41) { run++; if (run > longest) longest = run; } else run = 0;
  }
  assert.strictEqual(longest, 1024, `configured 1k but ${longest}-byte subpackets went out`);
});
