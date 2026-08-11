const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

// Load the browser module the way the page does: an IIFE that publishes onto
// window. No DOM is touched by the splitter, so a bare object suffices.
const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'zmodem-sentry.js'), 'utf8');
const win = {};
new Function('window', src)(win);
const { splitAtHeaderBoundaries } = win.ZmodemSentry;

// A ZRQINIT exactly as lrzsz `sz` puts it on the wire: "**<ZDLE>B" + 14 hex
// digits + CR + LF-with-high-bit + XON. CRC of an all-zero ZRQINIT is 0x0000.
const ZRQINIT = Uint8Array.from([
  0x2a, 0x2a, 0x18, 0x42,
  ...Array(14).fill(0x30),
  0x0d, 0x8a, 0x11,
]);

const bytes = (...parts) => Uint8Array.from(parts.flatMap(p => Array.from(p)));
const str = s => Uint8Array.from(Buffer.from(s, 'latin1'));

// The splitter returns { slices, tail }; `tail` is a header that began but did
// not finish in this buffer and must be carried into the next chunk.
const split = b => splitAtHeaderBoundaries(b);

test('chunk with no ZMODEM header passes through as a single slice', () => {
  const input = str('Welcome to the BBS!\r\nMain menu: ');
  const { slices, tail } = split(input);
  assert.strictEqual(slices.length, 1);
  assert.strictEqual(tail, null);
  assert.deepStrictEqual(Array.from(slices[0]), Array.from(input));
});

test('a header is emitted as a slice of its own', () => {
  // Headers must be isolated from the bytes before them: a session can end
  // part-way through a chunk, and only then can we tell whether the header
  // behind it is a stale retransmit or the next file's session opening.
  const input = bytes(str('rz\r'), ZRQINIT);
  const { slices, tail } = split(input);
  assert.strictEqual(slices.length, 2);
  assert.strictEqual(tail, null);
  assert.deepStrictEqual(Array.from(slices[0]), Array.from(str('rz\r')));
  assert.deepStrictEqual(Array.from(slices[1]), Array.from(ZRQINIT));
});

test('coalesced retransmit is cut so each header ends its own slice', () => {
  // The exact shape TCP delivered in the stall repro: banner + two ZRQINITs.
  const preamble = str('Ready to send via Zmodem.\r\nrz\r');
  const input = bytes(preamble, ZRQINIT, ZRQINIT);
  const { slices } = split(input);

  assert.strictEqual(slices.length, 3);
  assert.deepStrictEqual(Array.from(slices[0]), Array.from(preamble));
  assert.deepStrictEqual(Array.from(slices[1]), Array.from(ZRQINIT));
  assert.deepStrictEqual(Array.from(slices[2]), Array.from(ZRQINIT));
  // Nothing may be dropped or duplicated by the split.
  assert.deepStrictEqual(slices.flatMap(s => Array.from(s)), Array.from(input));
});

test('bytes trailing a header become their own slice', () => {
  const input = bytes(ZRQINIT, str('garbage'));
  const { slices } = split(input);
  assert.strictEqual(slices.length, 2);
  assert.deepStrictEqual(Array.from(slices[0]), Array.from(ZRQINIT));
  assert.deepStrictEqual(Array.from(slices[1]), Array.from(str('garbage')));
});

test('header straddling the chunk boundary is held back as tail', () => {
  const input = ZRQINIT.subarray(0, 10);
  const { slices, tail } = split(input);
  assert.strictEqual(slices.length, 0, 'nothing may be emitted yet');
  assert.deepStrictEqual(Array.from(tail), Array.from(input));
});

test('a partial signature at the very end is held back too', () => {
  // Cutting after just "*" or "**<ZDLE>" is too short for the 5-byte
  // signature scan to see, but still must not be released.
  for (const n of [1, 2, 3, 4]) {
    const input = bytes(str('menu '), ZRQINIT.subarray(0, n));
    const { slices, tail } = split(input);
    assert.deepStrictEqual(Array.from(tail), Array.from(ZRQINIT.subarray(0, n)), `n=${n}`);
    assert.deepStrictEqual(
      slices.flatMap(s => Array.from(s)), Array.from(str('menu ')), `n=${n}`);
  }
});

test('header lacking its CR/LF terminator is held, not emitted', () => {
  // zmodem.js cannot parse a hex header without CR LF, so emitting one here
  // produced an unparseable fragment that wedged the Sentry's cache.
  const bare = ZRQINIT.subarray(0, 18); // "**<ZDLE>B" + 14 hex, nothing after
  const { slices, tail } = split(bare);
  assert.strictEqual(slices.length, 0);
  assert.deepStrictEqual(Array.from(tail), Array.from(bare));
});

// Regression guard for the actual user-visible bug: with the coalesced chunk
// fed straight to zmodem.js the Sentry wedges forever, so drive the real
// library through the splitter and require a detection.
test('splitter lets zmodem.js detect a coalesced ZRQINIT pair', () => {
  const g = { window: null };
  g.window = g;
  new Function('window', 'module', 'exports',
    fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'vendor', 'zmodem.min.js'), 'utf8')
  )(g, {}, {});
  const Zmodem = g.Zmodem;

  function detectionsFor(feed) {
    let detects = 0;
    const sentry = new Zmodem.Sentry({
      to_terminal() {}, sender() {}, on_retract() {},
      on_detect() { detects++; },
    });
    feed(sentry);
    return detects;
  }

  const coalesced = bytes(str('rz\r'), ZRQINIT, ZRQINIT);
  const retransmits = 4;

  // Without the splitter: wedged — no detection, ever.
  assert.strictEqual(detectionsFor(s => {
    s.consume(coalesced);
    for (let i = 0; i < retransmits; i++) s.consume(ZRQINIT);
  }), 0);

  // With it: detected on the very first chunk.
  assert.ok(detectionsFor(s => {
    for (const slice of split(coalesced).slices) s.consume(slice);
  }) > 0);
});

// --- Live receive session: ZRQINIT retransmits ---------------------------
//
// A sender keeps retransmitting ZRQINIT until it sees our ZRINIT, so duplicates
// routinely land just after the receive session goes live. zmodem.js has no
// handler for them at that point and throws *without* consuming the bytes, so
// every later chunk re-parses the same header and throws again — the download
// dies with the header printed and nothing after it. ENiGMA½ triggers this;
// lrzsz on loopback usually does not.

const vm = require('node:vm');

// ENiGMA½ emits a plain LF where lrzsz sets the high bit, so cover both.
const ZRQINIT_LF = Uint8Array.from([
  0x2a, 0x2a, 0x18, 0x42,
  ...Array(14).fill(0x30),
  0x0d, 0x0a, 0x11,
]);

function liveBridge() {
  const g = {
    console: { log() {}, warn() {}, error() {}, debug() {} },
    setTimeout, clearTimeout, performance, Uint8Array, TextDecoder,
    Blob: class { constructor(c) { this.chunks = c; } },
    WebSocket: { OPEN: 1 },
  };
  g.window = g;
  vm.createContext(g);
  for (const f of ['cp437.js', 'xfer-util.js', 'vendor/zmodem.min.js', 'zmodem-sentry.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'public', 'js', f), 'utf8'), g, { filename: f });
  }
  g.window.ZmodemUI = {
    startXfer() {}, updateXfer() {}, endXfer() {},
    surfaceError() {}, surfaceDownload() {},
    promptUpload: () => Promise.resolve({ files: [] }),
  };
  const sent = [];
  const bridge = g.window.ZmodemSentry.createZmodemBridge({
    ws: { readyState: 1, send: b => sent.push(Buffer.from(b)) },
    term: { write() {} },
    config: { maxUploadBytes: 1 << 30, zmodemTimeoutSec: 30 },
    checkModemState() {}, flashLed() {},
  });
  return { bridge, sent };
}

// ZRINIT is what we send back to say "go ahead"; count them to prove the
// session came up exactly once.
const countZrinit = sent => {
  const hex = Buffer.concat(sent).toString('hex');
  return hex.split('2a2a18423031').length - 1;
};

// Ends a live receive session the way a real sender does: ZFIN then "OO".
// The Zmodem library lives inside each liveBridge sandbox, so build the ZFIN
// bytes once from a throwaway context.
const ZFIN = (() => {
  const g = {
    console: { log() {}, warn() {}, error() {}, debug() {} },
    setTimeout, clearTimeout, performance, Uint8Array, TextDecoder,
    Blob: class { constructor(c) { this.chunks = c; } }, WebSocket: { OPEN: 1 },
  };
  g.window = g;
  vm.createContext(g);
  for (const f of ['cp437.js', 'xfer-util.js', 'vendor/zmodem.min.js', 'zmodem-sentry.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'public', 'js', f), 'utf8'), g, { filename: f });
  }
  return Uint8Array.from(Buffer.from(g.Zmodem.Header.build('ZFIN').to_hex()));
})();
const endReceiveSession = bridge => bridge.consume(bytes(ZFIN, str('OO')));

for (const [label, hdr] of [['lrzsz (high-bit LF)', ZRQINIT], ['ENiGMA (plain LF)', ZRQINIT_LF]]) {
  test(`${label}: coalesced ZRQINIT pair starts the download without throwing`, () => {
    const { bridge, sent } = liveBridge();
    assert.doesNotThrow(() => {
      bridge.consume(bytes(str('\r\nrz\r'), hdr, hdr));
    });
    assert.strictEqual(countZrinit(sent), 1, 'should answer with exactly one ZRINIT');
  });

  test(`${label}: later ZRQINIT retransmits do not wedge the live session`, () => {
    const { bridge, sent } = liveBridge();
    bridge.consume(bytes(str('\r\nrz\r'), hdr));
    assert.strictEqual(countZrinit(sent), 1, 'session should be live');

    // Duplicates still in flight from before the sender saw our ZRINIT.
    assert.doesNotThrow(() => {
      for (let i = 0; i < 5; i++) bridge.consume(hdr);
    });
    assert.strictEqual(countZrinit(sent), 1, 'retransmits must not restart the session');
  });
}

// The bug that made batch downloads fail while single-file worked: the extra
// preamble a BBS prints for a tagged-file batch shifts the TCP chunk boundary
// into the middle of the ZRQINIT. Detection must survive a cut at *every*
// offset, not just the lucky ones.
test('detection survives a split at every byte offset', () => {
  const preamble = str('Disconnect after Download?  No  Yes  Quit  \r\n\r\nrz\r');

  for (const [variant, hdr] of [['high-bit LF', ZRQINIT], ['plain LF', ZRQINIT_LF]]) {
    for (const copies of [1, 2, 3]) {
      const stream = bytes(preamble, ...Array(copies).fill(hdr));

      for (let cut = 1; cut < stream.length; cut++) {
        const { bridge, sent } = liveBridge();
        assert.doesNotThrow(() => {
          bridge.consume(stream.subarray(0, cut));
          bridge.consume(stream.subarray(cut));
        }, `${variant} x${copies} cut=${cut}`);
        assert.strictEqual(
          countZrinit(sent), 1,
          `${variant} x${copies}: split at ${cut} should still answer with one ZRINIT`
        );
      }
    }
  }
});

// The failure this fixes: some BBSes (DOS DSZ/GSZ behind a telnet bridge) drive
// a multi-file download as one ZMODEM session *per file* — a fresh "rz" +
// ZRQINIT between each. Our retransmit-drop must not eat those, or only the
// first file (or a middle one) ever arrives.
test('a fresh ZRQINIT after a session ends starts a new download, not dropped', () => {
  const { bridge, sent } = liveBridge();

  // File 1 starts.
  bridge.consume(bytes(str('rz\r'), ZRQINIT_LF));
  assert.strictEqual(countZrinit(sent), 1, 'file 1 should be detected');

  // A retransmit of file 1's ZRQINIT while it is still live must be ignored.
  const before = countZrinit(sent);
  bridge.consume(ZRQINIT_LF);
  assert.strictEqual(countZrinit(sent), before, 'retransmit must not re-answer');

  // File 1's session ends, then file 2 opens with its own rz + ZRQINIT.
  endReceiveSession(bridge);
  bridge.consume(bytes(str('rz\r'), ZRQINIT_LF));
  assert.strictEqual(countZrinit(sent), before + 1, 'file 2 must be detected as a new session');

  // And a third.
  endReceiveSession(bridge);
  bridge.consume(bytes(str('rz\r'), ZRQINIT_LF));
  assert.strictEqual(countZrinit(sent), before + 2, 'file 3 must be detected too');
});
