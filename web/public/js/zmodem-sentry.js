// Bridges the WebSocket byte stream to zmodem.js + xterm.js. Owns byte-pump
// routing:
//   - Inbound (WS -> ??): non-ZMODEM bytes go to xterm via CP437 decode.
//     During a ZMODEM session, bytes are consumed by the session instead.
//   - Outbound (?? -> WS): xterm keystrokes when idle, session-emitted bytes
//     during transfer.
// Emits UI events via window.ZmodemUI.

(function () {
  // zmodem.js only raises a detection when the input it is handed *ends* with
  // a ZRQINIT/ZRINIT header. TCP gives us whatever it coalesced, so when `sz`
  // retransmits its ZRQINIT the two headers can share one chunk — and then
  // detection never fires. Worse, the surplus header stays in the Sentry's
  // cache, so every later retransmit is parsed one header behind and the
  // session never recovers: the terminal just shows repeating `rz**^XB00...`.
  //
  // Cutting the stream so each hex header ends a slice keeps the Sentry within
  // its documented contract. Chunks with no header pass through untouched.
  const ZM_HEX_START = [0x2a, 0x2a, 0x18, 0x42, 0x30]; // "**<ZDLE>B0"
  const ZM_HEX_DIGITS = 14; // 2 type + 8 data + 4 CRC

  // Returns { slices, tail }. `tail` is a header that started but did not
  // finish inside this buffer; the caller must hold it back and prepend it to
  // the next chunk. Letting a partial header through is what made this fail
  // intermittently: the Sentry caches the fragment, the rest of the stream
  // lands behind it, and any further header in that cache wedges detection.
  function splitAtHeaderBoundaries(bytes) {
    const slices = [];
    let start = 0;

    for (let i = 0; i + ZM_HEX_START.length <= bytes.length; i++) {
      if (bytes[i] !== ZM_HEX_START[0]) continue;
      let match = true;
      for (let k = 1; k < ZM_HEX_START.length; k++) {
        if (bytes[i + k] !== ZM_HEX_START[k]) { match = false; break; }
      }
      if (!match) continue;

      let end = i + 4 + ZM_HEX_DIGITS; // through the final hex digit
      // A hex header always ends CR LF, and zmodem.js cannot parse one without
      // that terminator — so the header is only "complete" once both are here.
      // Cutting a slice at the last hex digit produced an unparseable fragment
      // that then wedged the Sentry's cache.
      const needTrailer = end + 1 >= bytes.length ||
        (bytes[end] === 0x0d && end + 1 >= bytes.length);
      if (end > bytes.length || needTrailer) {
        if (i > start) slices.push(bytes.subarray(start, i));
        return { slices, tail: bytes.subarray(i) };
      }
      if (bytes[end] === 0x0d) end++;                       // CR
      if (bytes[end] === 0x0a || bytes[end] === 0x8a) end++; // LF (lrzsz sets the high bit)
      // XON is absent on ZACK/ZFIN, so take it only if it is already here
      // rather than waiting for a byte that may never come.
      if (bytes[end] === 0x11 || bytes[end] === 0x91) end++;

      // Emit the header as a slice of its own. A session can end part-way
      // through a chunk, and whether the header that follows is a stale
      // retransmit or the start of the next file's session depends on state
      // that only exists once the preceding bytes have been consumed.
      if (i > start) slices.push(bytes.subarray(start, i));
      slices.push(bytes.subarray(i, end));
      start = end;
      i = end - 1;
    }

    // The buffer may also stop part-way through the 5-byte signature itself,
    // which is too short for the loop above to have seen.
    const room = bytes.length - start;
    for (let n = Math.min(ZM_HEX_START.length - 1, room); n >= 1; n--) {
      let ok = true;
      for (let k = 0; k < n; k++) {
        if (bytes[bytes.length - n + k] !== ZM_HEX_START[k]) { ok = false; break; }
      }
      if (!ok) continue;
      if (bytes.length - n > start) slices.push(bytes.subarray(start, bytes.length - n));
      return { slices, tail: bytes.subarray(bytes.length - n) };
    }

    if (start < bytes.length) slices.push(bytes.subarray(start));
    return { slices, tail: null };
  }

  // A ZRQINIT that lands while a receive session is still live is a retransmit
  // the sender emitted before it saw our ZRINIT. zmodem.js has no handler for
  // one then and throws *without* consuming the bytes, so every later chunk
  // re-parses it and throws again, wedging the download for good.
  //
  // Once that session has ended the very same bytes mean the opposite: BBSes
  // that drive a batch as one ZMODEM session per file send a fresh "rz" +
  // ZRQINIT for each. Those must reach the Sentry or only one file arrives.
  // Headers are sliced out individually so this decision is made per header,
  // after everything before it has been consumed.
  //
  // Matching raw bytes is safe: ZMODEM escapes ZDLE inside data subpackets, so
  // an unescaped 0x18 is always a frame marker, never file content.
  const ZRQINIT_SIG = [0x2a, 0x2a, 0x18, 0x42, 0x30, 0x30]; // "**<ZDLE>B00"
  const ZRINIT_SIG = [0x2a, 0x2a, 0x18, 0x42, 0x30, 0x31]; // "**<ZDLE>B01"

  function matchesSig(chunk, sig) {
    if (chunk.length < sig.length) return false;
    for (let k = 0; k < sig.length; k++) {
      if (chunk[k] !== sig[k]) return false;
    }
    return true;
  }

  function isZrqinitHeader(chunk) { return matchesSig(chunk, ZRQINIT_SIG); }
  function isZrinitHeader(chunk) { return matchesSig(chunk, ZRINIT_SIG); }

  // Header types, so a trace can say what the receiver actually said. Every
  // inbound header except ZRINIT used to be consumed in silence, which left the
  // decisive question about a failing board unanswerable from a pasted session:
  // a `ZRPOS 0` answering our ZFILE is the offer being *accepted*, while the
  // same header at any other moment means "I am lost, start again", and the two
  // are indistinguishable unless you can see the order they arrived in.
  const HEADER_NAMES = [
    'ZRQINIT', 'ZRINIT', 'ZSINIT', 'ZACK', 'ZFILE', 'ZSKIP', 'ZNAK', 'ZABORT',
    'ZFIN', 'ZRPOS', 'ZDATA', 'ZEOF', 'ZFERR', 'ZCRC', 'ZCHALLENGE', 'ZCOMPL',
    'ZCAN', 'ZFREECNT', 'ZCOMMAND', 'ZSTDERR',
  ];

  function headerName(type) {
    return HEADER_NAMES[type] || `type 0x${type.toString(16)}`;
  }

  const MAX_TRACED_HEADERS = 24;

  // ZMODEM data subpacket sizes. 1 KiB is what the specification says; 8 KiB is
  // the "ZMODEM-8k" (ZedZap) variant that DSZ introduced and every modern
  // receiver runs. ENiGMA½ names its two default upload protocols exactly that —
  // "ZModem 8k (SEXYZ)" (`sexyz -telnet -8 rz`) and "ZModem 8k" (lrzsz's `rz`) —
  // and both assemble subpackets into an 8192-byte buffer. 8192 is also
  // zmodem.js's own MAX_CHUNK_LENGTH, so it is the largest slice we can hand
  // _send_file_part and still get one subpacket out of it.
  const SUBPACKET_1K = 1024;
  const SUBPACKET_8K = 8192;

  // Nothing in ZMODEM negotiates the *sender's* subpacket size — the sender
  // picks and the receiver copes, which is why "ZMODEM-8k" is a protocol name on
  // a menu rather than a capability flag in the handshake. **So auto means
  // 1 KiB**, the size the specification names and the only one every receiver
  // has been observed to take.
  //
  // This used to infer 8 KiB from a receiver advertising no window, on the
  // theory that a streaming receiver has no buffer to overrun. Two boards agreed
  // and one did not: MajorBBS advertises buffer 0 and still answers an 8 KiB
  // subpacket with `ZRPOS 0`, then accepts the identical file once the fallback
  // drops to 1 KiB. The inference was wrong, and the trade is lopsided —
  // guessing 8 KiB on a board that refuses it costs a full re-send of everything
  // already in flight (megabytes, on a large file), while guessing 1 KiB on a
  // board that would have taken 8 KiB costs about 15% throughput, measured
  // against real `rz` over 8 MiB.
  //
  // So 8 KiB is opt-in: pick "Blocks: 8k" in the upload prompt for a board known
  // to take it, or set ZMODEM_BLOCK_SIZE for a deployment-wide default.
  function chooseBlockSize(receiverWindow, configured) {
    if (configured === SUBPACKET_1K || configured === SUBPACKET_8K) return configured;
    return SUBPACKET_1K;
  }

  // zmodem.js only supports receivers that advertise a zero buffer size, i.e.
  // pure streaming. DSZ/GSZ (and other DOS ZMODEM implementations) advertise a
  // non-zero receive buffer in their ZRINIT, which makes _consume_ZRINIT throw
  // "Buffer size (...) is unsupported". The Sentry swallows that throw, so an
  // upload to such a BBS never even opens the file picker — the ZRINIT just
  // echoes to the terminal and `rz` retransmits forever.
  //
  // So we rewrite an inbound ZRINIT to advertise buffer 0, preserving its
  // capability flags, and fix up the CRC. Keeping this in our code leaves the
  // vendored library untouched.
  //
  // The rewrite is a lie, and on its own it is only half a fix: the receiver
  // asked for windowing and we told it that it didn't. `applyWindowedSend`
  // below pays that debt back by honouring the real figure. Read the two
  // together — rewriting alone streams a whole file at a receiver that asked us
  // not to, which it survives right up until it doesn't.
  //
  // Hex header layout after "**<ZDLE>B": TYPE(2) ZP0(2) ZP1(2) ZP2(2) ZP3(2)
  // CRC(4), each pair an ASCII hex byte. get_buffer_size() reads ZP0/ZP1.
  const HEX_DIGITS = '0123456789abcdef';
  function hexVal(b) {
    if (b >= 0x30 && b <= 0x39) return b - 0x30;
    if (b >= 0x61 && b <= 0x66) return b - 0x61 + 10;
    if (b >= 0x41 && b <= 0x46) return b - 0x41 + 10;
    return -1;
  }
  function pushHexByte(arr, v) {
    arr.push(HEX_DIGITS.charCodeAt((v >> 4) & 0xf), HEX_DIGITS.charCodeAt(v & 0xf));
  }

  function adaptZrinitForSender(chunk) {
    const H = 4; // past "**<ZDLE>B"
    if (chunk.length < H + 14) return chunk; // header not fully present
    const bytes = [];
    for (let i = 0; i < 5; i++) { // TYPE + ZP0..ZP3
      const hi = hexVal(chunk[H + i * 2]);
      const lo = hexVal(chunk[H + i * 2 + 1]);
      if (hi < 0 || lo < 0) return chunk; // not clean hex — leave it alone
      bytes.push((hi << 4) | lo);
    }
    if (bytes[1] === 0 && bytes[2] === 0) return chunk; // already streaming
    bytes[1] = 0; // ZP0
    bytes[2] = 0; // ZP1
    const crc = window.Zmodem.CRC.crc16(bytes);
    const rebuilt = [chunk[0], chunk[1], chunk[2], chunk[3]]; // "**<ZDLE>B"
    for (const b of bytes) pushHexByte(rebuilt, b);
    pushHexByte(rebuilt, crc[0]);
    pushHexByte(rebuilt, crc[1]);
    for (let i = H + 14; i < chunk.length; i++) rebuilt.push(chunk[i]); // trailer
    return Uint8Array.from(rebuilt);
  }

  // Reads the receive-window size a ZRINIT advertises, in bytes. 0 means the
  // receiver streams and needs no windowing. Must be called before
  // adaptZrinitForSender, which zeroes the very field this reads.
  function readZrinitWindow(chunk) {
    const H = 4; // past "**<ZDLE>B"
    if (chunk.length < H + 14) return 0;
    const zp0hi = hexVal(chunk[H + 2]), zp0lo = hexVal(chunk[H + 3]);
    const zp1hi = hexVal(chunk[H + 4]), zp1lo = hexVal(chunk[H + 5]);
    if (zp0hi < 0 || zp0lo < 0 || zp1hi < 0 || zp1lo < 0) return 0;
    // ZP0 is the low byte of the size, ZP1 the high byte.
    return ((zp0hi << 4) | zp0lo) | (((zp1hi << 4) | zp1lo) << 8);
  }

  // Teaches a send session to respect the window its receiver asked for.
  //
  // zmodem.js's sender is firehose-only by design — "Sender opens the firehose
  // … all ZCRCG (!end/!ack) until the end" — which is why _consume_ZRINIT
  // rejects a windowed receiver outright instead of slowing down for one. Since
  // we rewrite that ZRINIT to get past the check, nothing else will pace the
  // send, and a receiver that advertised 8192 gets the entire file in one
  // burst. Searchlight BBS cancels with CAN*8 when that happens; the browser
  // still shows 100%, because progress counts bytes handed to zmodem.js rather
  // than bytes the receiver acknowledged.
  //
  // So: send at most `windowSize` bytes, close the frame with ZCRCW ("frame
  // ends, ZACK expected"), and wait for the ZACK before continuing. The library
  // documents this exact extension point on _send_interim_file_piece — "for now
  // the promise is always resolved, but in the future we can make it only
  // resolve once we've gotten acknowledgement" — so overriding the method on
  // the session instance keeps the vendored file untouched.
  //
  // send_offer binds this method when it builds the Transfer, so the override
  // has to be installed before send_offer runs.
  //
  // One wrinkle: Transfer.send() calls its send function and throws the result
  // away — fine while that result was always an already-resolved promise, but
  // ours resolves only on ZACK. So the in-flight wait is also parked on the
  // session, where sendChunk() below can await it.
  // A ZACK carries the receiver's file offset, but zmodem.js models ZACK as a
  // plain Header rather than a ZOffsetHeader ("we really need this header only
  // to respond to ZSINIT"), so there is no get_offset() to call. Unpack the
  // four payload bytes directly, and return null if the shape is not what we
  // expect rather than inventing a number the caller would then trust.
  function readAckOffset(hdr) {
    const b = hdr && hdr._bytes4;
    if (!b || b.length < 4) return null;
    return ((b[0] | (b[1] << 8) | (b[2] << 16) | (b[3] << 24)) >>> 0);
  }

  function applyWindowedSend(zsession, windowSize) {
    if (!windowSize) return;
    let unacked = 0;
    let sentTotal = 0;
    let pending = Promise.resolve();

    zsession._windowDrain = () => pending;
    // How far the receiver has actually confirmed. handleSend checks this
    // against the file size instead of trusting the post-ZEOF ZRINIT.
    // A bare ZACK carries a zero offset (zmodem.js defaults _bytes4 to four
    // zero bytes and not every receiver fills it in), so the count is the
    // reliable signal and the offset only corroborates when it is non-zero.
    zsession._ackedOffset = 0;
    zsession._ackCount = 0;
    // Set by handleSend for the last slice of the file.
    zsession._ackFinalPiece = false;

    zsession._send_interim_file_piece = function (bytesObj) {
      const sess = this;
      let offset = 0;

      const step = () => {
        if (offset >= bytesObj.length) return Promise.resolve();

        // Never overshoot the window, however the caller happens to chunk.
        const take = Math.min(windowSize - unacked, bytesObj.length - offset);
        const slice = bytesObj.slice(offset, offset + take);
        offset += take;
        unacked += take;
        sentTotal += take;

        // Close the frame when the window fills — and always on the file's last
        // slice, even if the window never filled. Without that second case a
        // file smaller than the advertised window goes out wholly
        // unacknowledged: zmodem.js sends every subpacket ZCRCG and the final
        // one ZCRCE, both "no ack" ("Sender opens the firehose … all ZCRCG
        // until the end, when we send a ZCRCE"). The sender then holds no
        // evidence that a single byte arrived, and the only thing it waits for
        // is a ZRINIT after ZEOF — which is byte-identical to the ZRINIT a
        // receiver sends when it has given up and re-announced itself. A
        // transfer that landed nowhere therefore reports success. One ZACK
        // before ZEOF is what makes the two cases distinguishable.
        const lastOfFile = sess._ackFinalPiece && offset >= bytesObj.length;
        if (unacked < windowSize && !lastOfFile) {
          sess._send_file_part(slice, 'no_end_no_ack'); // ZCRCG, keep streaming
          return step();
        }

        // Register the handler before sending: on a fast link the ZACK can come
        // back synchronously, and an unhandled header makes zmodem.js throw.
        const acked = new Promise((resolve, reject) => {
          sess._next_header_handler = {
            ZACK: hdr => {
              const off = readAckOffset(hdr);
              if (off !== null) sess._ackedOffset = off;
              sess._ackCount++;
              resolve();
            },
            // The receiver rewinds to an offset when a frame fails its CRC. We
            // stream straight from the caller's buffer and keep nothing to
            // replay, so report it rather than silently truncating the file.
            ZRPOS: hdr => reject(new Error(
              'receiver asked to resume at offset ' + hdr.get_offset() + '; cannot rewind'
            )),
            ZRINIT: () => reject(new Error('receiver ended the transfer early')),
          };
        });

        sess._send_file_part(slice, 'end_ack'); // ZCRCW
        // ZCRCW closes the frame, so the next subpacket needs a fresh ZDATA
        // header carrying the offset we have reached.
        sess._sent_ZDATA = false;

        return acked.then(() => { unacked = 0; return step(); });
      };

      pending = step();
      return pending;
    };
  }

  // Hands one chunk to the transfer and waits for the sender to be ready for
  // the next. Transfer.send() is synchronous and returns nothing, so awaiting
  // it directly paces nothing; a windowed session parks its ZACK wait on the
  // session and that is what actually has to be awaited.
  function sendChunk(zsession, xfer, slice) {
    xfer.send(slice);
    return zsession._windowDrain ? zsession._windowDrain() : Promise.resolve();
  }

  // Describes a hex header as the peer actually sent it: the four data bytes,
  // and for a ZRINIT the receive-buffer size it is asking us to respect. The
  // terminal only ever shows the post-rewrite bytes, so a receiver that wants
  // windowing is otherwise indistinguishable from a streaming one. Returns null
  // when the chunk is not a parseable header. Diagnostic only — nothing
  // downstream reads the result.
  // Unpacks a hex header into its type byte and four data bytes, or null when
  // the chunk is not one.
  function parseHexHeader(chunk) {
    const H = 4; // past "**<ZDLE>B"
    if (chunk.length < H + 10) return null;
    const b = [];
    for (let i = 0; i < 5; i++) {
      const hi = hexVal(chunk[H + i * 2]);
      const lo = hexVal(chunk[H + i * 2 + 1]);
      if (hi < 0 || lo < 0) return null;
      b.push((hi << 4) | lo);
    }
    return { type: b[0], data: b.slice(1) };
  }

  // ZP0..ZP3 are a little-endian file offset on the headers that carry one.
  const CARRIES_OFFSET = { 3: true, 9: true, 10: true, 11: true }; // ZACK ZRPOS ZDATA ZEOF

  // One compact line naming an inbound header, for the trace.
  function describeInbound(chunk) {
    const hdr = parseHexHeader(chunk);
    if (!hdr) return null;
    let line = '<- ' + headerName(hdr.type);
    if (CARRIES_OFFSET[hdr.type]) {
      const off = (hdr.data[0] | (hdr.data[1] << 8) | (hdr.data[2] << 16) | (hdr.data[3] << 24)) >>> 0;
      line += ' offset=' + off;
    }
    return line;
  }

  function describeHeader(label, chunk) {
    const H = 4; // past "**<ZDLE>B"
    if (chunk.length < H + 10) return null;
    const b = [];
    for (let i = 0; i < 5; i++) {
      const hi = hexVal(chunk[H + i * 2]);
      const lo = hexVal(chunk[H + i * 2 + 1]);
      if (hi < 0 || lo < 0) return null;
      b.push((hi << 4) | lo);
    }
    const hex = Array.from(chunk).map(v => v.toString(16).padStart(2, '0')).join(' ');
    // ZP0 is the low byte of the advertised buffer size, ZP1 the high byte.
    const bufsize = b[1] | (b[2] << 8);
    return `[zmodem] ${label} buffer=${bufsize}` +
      (bufsize ? ` (windowed receiver - pacing sends to ${bufsize} bytes/ZACK)` : ' (streaming)') +
      ` flags=0x${b[4].toString(16)} raw=<${hex}>`;
  }

  function createZmodemBridge({ ws, term, config, checkModemState, flashLed }) {
    let session = null;
    let idleTimer = null;
    let currentXferBytes = 0;
    let currentXferStart = 0;
    // Newest unconfirmed send detection, and the file picker it is waiting on.
    let pendingSend = null;
    let uploadPrompt = null;
    // A header that straddled the last chunk boundary, waiting for its rest.
    let headerTail = null;
    let tailFlushTimer = null;
    // Whether the live receive session has already seen its file offer. Until
    // it has, a stray ZRQINIT is a retransmit of this same session's start; once
    // it has, a ZRQINIT means the *next* file is beginning.
    let sessionSawOffer = false;
    // Receive window the BBS last advertised, read before we rewrite it away.
    let receiverWindow = 0;
    // Inbound headers named on the terminal so far. Capped: a peer that spins
    // retransmitting would otherwise bury the session text it is meant to
    // explain, and the first handful are the ones that carry the diagnosis.
    let tracedHeaders = 0;

    // `seconds` overrides the configured idle window for a wait we know is
    // legitimately longer than one (see the ZEOF acknowledgement in handleSend).
    function armTimeout(seconds) {
      clearTimeout(idleTimer);
      const secs = seconds || config.zmodemTimeoutSec;
      idleTimer = setTimeout(() => {
        if (!session) return;
        // Say so. This path used to end a transfer in complete silence, which is
        // why every failing session anyone pasted stopped mid-sentence with no
        // way to tell a timeout from the BBS hanging up.
        trace(`TIMED OUT - ${secs}s with nothing from the receiver`);
        window.ZmodemUI.endXfer({ status: 'timeout' });
        try { session.abort(); } catch (e) { /* session may already be dead */ }
        session = null;
      }, secs * 1000);
    }

    function disarmTimeout() { clearTimeout(idleTimer); idleTimer = null; }

    function sendToWS(octets) {
      if (ws.readyState === WebSocket.OPEN) {
        const buf = octets instanceof Uint8Array ? octets : new Uint8Array(octets);
        ws.send(buf);
      }
    }

    // ws.send() never blocks: it appends to the socket's send buffer and returns.
    // Nothing above it blocked either, so the upload loop used to hand zmodem.js
    // the entire file as fast as JavaScript could escape it, `updateXfer` painted
    // 100%, and the transfer had barely started. A 116 MB upload to ENiGMA½ died
    // exactly there — the loop finished in seconds, printed "all 121779977 bytes
    // sent", and nothing re-armed the idle timer after that, so 30s later the
    // session was aborted (silently: the timeout path does not trace) while the
    // first megabyte was still on the wire.
    //
    // So the loop waits for the socket to drain before queueing more. Two things
    // fall out of that beyond bounded memory: progress now tracks bytes the
    // browser has actually handed to the network, and the idle timer measures
    // the link rather than the loop.
    const WS_HIGH_WATER = 1 << 20; // 1 MiB queued keeps the link fed
    const WS_LOW_WATER = 1 << 18;  // resume at 256 KiB so it never runs dry

    function wsBacklog() {
      // Absent in tests and in any environment that doesn't report it; treating
      // that as "nothing queued" keeps the loop at its old, unpaced behaviour.
      return typeof ws.bufferedAmount === 'number' ? ws.bufferedAmount : 0;
    }

    // Waits until the socket's backlog falls to `target`. The idle timer is
    // re-armed only when the backlog has actually *fallen* since the last poll,
    // so a link that has genuinely stalled still times out instead of being held
    // open indefinitely by its own queue.
    async function drainWS(zsession, target) {
      const floor = target === undefined ? WS_LOW_WATER : target;
      let last = Infinity;
      while (wsBacklog() > floor) {
        if (ws.readyState !== WebSocket.OPEN) {
          throw new Error('the connection closed mid-transfer');
        }
        if (session !== zsession) {
          // Either armTimeout() fired or feed() hit a protocol error; both have
          // already said so and ended the strip, so don't relabel it.
          const err = new Error('the session ended while the link drained');
          err.alreadyReported = true;
          throw err;
        }
        const now = wsBacklog();
        if (now < last) { last = now; armTimeout(); }
        await new Promise(r => setTimeout(r, 20));
      }
    }

    // The Sentry has to be replaceable: some senders start the next file's
    // session without ever formally closing the previous one, so we tear the
    // stale session down and hand its trailing ZRQINIT to a fresh Sentry.
    let sentry = makeSentry();

    function makeSentry() {
      return new Zmodem.Sentry({
        to_terminal(octets) {
          // Non-ZMODEM bytes: decode via CP437 and write to xterm.
          const bytes = octets instanceof Uint8Array ? octets : new Uint8Array(octets);
          const text = window.CP437.cp437ToUtf8(bytes);
          term.write(text);
          // Scan every chunk, not just small ones: result codes get coalesced
          // with the payload around them (CONNECT rides in with the BBS banner),
          // so chunk size says nothing about whether a code is present.
          checkModemState(text);
          flashLed('rd');
        },
        sender(octets) { sendToWS(octets); },
        on_retract() { /* False positive; nothing to do. */ },
        on_detect(detection) {
          if (detection.get_session_role() === 'send') {
            // Don't claim the stream yet. `rz` resends ZRINIT every ~10s until
            // it gets a ZFILE, and zmodem.js throws once a live session sees a
            // second header — which is guaranteed here, because the ZFILE isn't
            // sent until the user has picked a file. Hold the newest detection
            // instead and confirm it only when we actually have bytes to send.
            pendingSend = detection;
            beginUpload();
            return;
          }
          handleReceive(confirmSession(detection));
        },
      });
    }

    function clearTailFlush() {
      if (tailFlushTimer) { clearTimeout(tailFlushTimer); tailFlushTimer = null; }
    }

    // Never sit on held-back bytes forever: "*" and "**" are ordinary BBS text
    // as often as not, and even a real fragment must not be swallowed if its
    // remainder never arrives. The rest of a header in flight lands in the very
    // next segment, far inside this window.
    function scheduleTailFlush() {
      clearTailFlush();
      if (!headerTail) return;
      tailFlushTimer = setTimeout(() => {
        tailFlushTimer = null;
        const tail = headerTail;
        headerTail = null;
        if (tail) feed(tail);
      }, 100);
    }

    // Single entry point into the Sentry, so a ZRQINIT is always classified
    // against the current session state no matter which path delivered it.
    function feed(chunk) {
      // The receiver's cancel. It reaches the terminal as a run of CP437 arrows
      // and reads as line noise; name it so a pasted session says who gave up.
      let cans = 0;
      for (const b of chunk) {
        if (b === 0x18 && ++cans >= 5) { trace('receiver sent CAN*8 - it cancelled the transfer'); break; }
        if (b !== 0x18) cans = 0;
      }
      // Name every header the peer sends. Only ZRINIT used to reach the
      // terminal, so a pasted session could not show whether a `ZRPOS 0` was the
      // ZFILE being accepted or the receiver saying it had lost sync — the two
      // differ only by where they fall in the exchange.
      if (tracedHeaders < MAX_TRACED_HEADERS && !isZrinitHeader(chunk)) {
        const line = describeInbound(chunk);
        if (line) { tracedHeaders++; trace(line); }
      }
      if (session && session.type === 'receive' && isZrqinitHeader(chunk)) {
        // Before the offer arrives, a ZRQINIT is the sender re-announcing this
        // same session (it hasn't seen our ZRINIT yet) — drop it, or zmodem.js
        // throws "Unhandled header" and wedges. After the offer, the file is
        // already being delivered, so a ZRQINIT is the next file's session
        // starting. Some senders never send ZFIN between files, so the old
        // session won't have closed itself; tear it down and let a fresh Sentry
        // detect the new one.
        if (!sessionSawOffer) return;
        resetSession();
      }
      // Make any ZRINIT palatable to zmodem.js before it reaches the Sentry,
      // whether we're detecting an upload or the receiver is re-announcing
      // between files during one.
      if (isZrinitHeader(chunk)) {
        // Read the window before the rewrite destroys it. Only a non-zero
        // figure updates it: a receiver re-announcing itself mid-exchange (the
        // ZRINIT that answers our ZSINIT) may report 0, and dropping back to
        // firehose halfway through is the failure this exists to prevent.
        const win = readZrinitWindow(chunk);
        if (win > 0) receiverWindow = win;
        // The terminal copy is what makes this diagnosable: a failed transfer
        // gets reported by pasting the session text, and the rewritten header
        // there looks identical whatever the BBS actually asked for.
        const desc = describeHeader('ZRINIT', chunk);
        if (desc) {
          console.log(desc);
          term.write('\r\n' + desc + '\r\n');
        }
        // A receiver repeats its ZRINIT until it gets a ZFILE, so one can land at
        // any moment during the opening exchange. zmodem.js clears
        // `_next_header_handler` the instant it dispatches a header and installs
        // the next one on a *microtask* — but our splitter feeds every header of
        // a chunk in one synchronous turn, so a ZACK and the ZRINIT behind it in
        // the same TCP segment hit that null window and `_consume_header` throws
        // "Cannot read properties of null (reading 'ZRINIT')".
        //
        // That is precisely what SEXYZ (ENiGMA½'s "ZModem 8k (SEXYZ)") does: it
        // answers our ZSINIT with a ZACK and then loops straight back to
        // re-announcing itself, so the two arrive together and the upload dies
        // before the ZFILE — while zmodem.js's own promise chain carries on and
        // sends it anyway, which is why the receiver's ZRPOS then lands on a torn
        // -down session and prints as line noise.
        //
        // A send session only ever *wants* a ZRINIT where it has asked for one
        // (after ZEOF, and it registers that handler before sending the ZEOF), so
        // anything else is a keep-alive and dropping it is what the sender would
        // have done had it been able to see it.
        if (session && session.type === 'send') {
          const handler = session._next_header_handler;
          if (!handler || !handler.ZRINIT) return;
        }
        chunk = adaptZrinitForSender(chunk);
      }
      if (chunk.length === 0) return;
      try {
        sentry.consume(chunk);
      } catch (err) {
        // zmodem.js throws on any header its current handler does not list, and
        // a receiver that repeats its ZRINIT while we wait for ZRPOS is enough
        // to trigger it (lrzsz does exactly that when it does not like an
        // offer). The throw escapes bridge.consume() into the WebSocket message
        // handler, where nothing catches it — so the bridge stops pumping bytes
        // and the terminal goes dead with nothing on screen to say why, which
        // looks identical to the BBS having hung up.
        //
        // Report it and reset instead. This does not make the transfer succeed;
        // it turns a silent death into a stated failure and leaves the terminal
        // usable.
        trace(`protocol error: ${err && err.message ? err.message : err}`);
        const hadSession = session !== null;
        disarmTimeout();
        session = null;
        sessionSawOffer = false;
        sentry = makeSentry();
        if (hadSession) window.ZmodemUI.endXfer({ status: 'aborted' });
      }
    }

    // Abandon the current receive session and start clean. The stale session's
    // socket is the same one the next file uses, so we don't abort() (that would
    // send a cancel to the sender); we just drop our references and rebuild the
    // Sentry so its internal _zsession no longer points at the dead session.
    function resetSession() {
      disarmTimeout();
      session = null;
      sessionSawOffer = false;
      sentry = makeSentry();
    }

    function confirmSession(detection) {
      session = detection.confirm();
      sessionSawOffer = false;
      armTimeout();
      session.on('session_end', () => {
        disarmTimeout();
        session = null;
      });
      return session;
    }

    function handleReceive(zsession) {
      let batchIndex = 0;

      zsession.on('offer', xfer => {
        sessionSawOffer = true;
        batchIndex++;
        const details = xfer.get_details();
        const chunks = [];
        let overCap = false;
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
          if (overCap) return;
          const buf = payload instanceof Uint8Array ? payload : new Uint8Array(payload);
          chunks.push(buf);
          currentXferBytes += buf.length;
          if (currentXferBytes > config.maxUploadBytes) {
            // Malicious/misconfigured sender can OOM the tab if we accept
            // unbounded input. Same cap as uploads keeps memory bounded.
            overCap = true;
            window.ZmodemUI.surfaceError(
              `${details.name}: exceeds ${window.XferUtil.formatBytes(config.maxUploadBytes)}`
            );
            try { xfer.skip(); } catch (e) { /* fall through to session.abort */ }
            try { session && session.abort(); } catch (e) { /* nop */ }
            return;
          }
          const elapsed = (performance.now() - currentXferStart) / 1000;
          const cps = elapsed > 0 ? currentXferBytes / elapsed : 0;
          window.ZmodemUI.updateXfer({ bytes: currentXferBytes, cps });
          armTimeout();
        });

        xfer.accept().then(() => {
          if (overCap) return;
          disarmTimeout();
          window.ZmodemUI.endXfer({ status: 'done' });
          const blob = new Blob(chunks);
          window.ZmodemUI.surfaceDownload({ filename: details.name, blob });
        }).catch(err => {
          disarmTimeout();
          console.error('ZMODEM receive error', err);
          window.ZmodemUI.endXfer({ status: 'aborted' });
          try { session && session.abort(); } catch (e) { /* nop */ }
        });
      });

      zsession.start();
    }

    // Opens the file picker once per upload offer. Repeat ZRINITs arriving
    // while it is open just refresh pendingSend, so the session we finally
    // confirm is the one the Sentry still considers live.
    function beginUpload() {
      if (uploadPrompt) return;
      uploadPrompt = window.ZmodemUI.promptUpload(config.maxUploadBytes).then(files => {
        const detection = pendingSend;
        pendingSend = null;
        uploadPrompt = null;

        // The BBS gave up, or the stream turned out not to be ZMODEM after all.
        if (!detection || !detection.is_valid()) return undefined;
        if (files.length === 0) {
          try { detection.deny(); } catch (e) { /* nothing left to abort */ }
          return undefined;
        }
        const zsession = confirmSession(detection);
        // Must happen before send_offer, which binds the send method into the
        // Transfer it hands back.
        applyWindowedSend(zsession, receiverWindow);
        return handleSend(zsession, files);
      }).catch(err => {
        pendingSend = null;
        uploadPrompt = null;
        disarmTimeout();
        console.error('ZMODEM upload error', err);
        window.ZmodemUI.endXfer({ status: 'aborted' });
      });
    }

    // Upload progress narration. An upload that fails against a real BBS is
    // reported by pasting the session text, and until it says otherwise every
    // failure looks identical there: the ZRINIT, then the receiver's CAN*8.
    // Outbound headers are never echoed and in-session inbound bytes are
    // consumed rather than displayed, so those two lines sit next to each other
    // whether the transfer died on the first handshake or after the last byte.
    // These milestones are what distinguish those cases.
    function trace(msg) {
      console.log('[zmodem] ' + msg);
      term.write('\r\n[zmodem] ' + msg + '\r\n');
    }

    // The maximum number of times a receiver may rewind us before we conclude
    // the data is not getting through for a reason resending will not cure.
    // The first rewind drops us to 1 KiB subpackets; the rest cover a genuine
    // transient. Each one resends from the requested offset, so an unbounded
    // count would mean an unbounded amount of wasted upload.
    const MAX_REWINDS = 3;

    function handleSend(zsession, files) {
      let stage = 'starting';
      return (async () => {
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
          const offer = {
            name: file.name,
            size: file.size,
            mtime: Math.floor(file.lastModified / 1000),
            bytes_remaining: files.slice(i).reduce((s, f) => s + f.size, 0),
          };
          // zmodem.js rejects a literal 0 for files_remaining — "none left"
          // is expressed by omitting the field. Sending it as 0 (which is the
          // last file of every batch, so every single-file upload) aborted the
          // transfer before any data went out.
          const remaining = files.length - i - 1;
          if (remaining > 0) offer.files_remaining = remaining;
          // The receiver did not ask for ESCCTL, so zmodem.js detours through a
          // ZSINIT here and blocks until the receiver acknowledges it. That is
          // the first place a transfer can die without anything reaching the
          // terminal, so say so before and after.
          stage = 'sending offer (ZSINIT/ZFILE)';
          trace(`offering ${file.name} (${file.size} bytes)`);
          const xfer = await zsession.send_offer(offer);
          stage = 'streaming data';
          if (!xfer) {
            // send_offer resolves undefined for exactly one reason: the
            // receiver answered our ZFILE with ZSKIP. That is a refusal, not a
            // delivery — reporting it as 'done' told the user an upload had
            // succeeded when the BBS never accepted a byte of it. Searchlight
            // skips a name it already holds ("Checking for duplicate
            // filenames..." precedes the offer), so the usual cause is that the
            // file is already there from an earlier attempt.
            trace(`receiver REFUSED ${file.name} (ZSKIP) - it was not uploaded. ` +
              'A BBS commonly skips a filename it already has; try a different name.');
            window.ZmodemUI.surfaceError(`${file.name}: refused by the BBS (already there?)`);
            window.ZmodemUI.endXfer({ status: 'skipped' });
            continue;
          }
          // One ZMODEM data subpacket per slice. The size is the caller's choice
          // from the upload prompt, falling back to chooseBlockSize(): a windowed
          // receiver needs the 1 KiB the specification names (4 KB made
          // Searchlight BBS 5.1 answer every ZEOF with `ZRPOS 0` until the
          // transfer was cancelled), a streaming one is offered the 8 KiB
          // "ZMODEM-8k" its protocol menu advertises. 8192 is zmodem.js's
          // MAX_CHUNK_LENGTH, so a slice still maps to exactly one subpacket.
          let CHUNK = chooseBlockSize(receiverWindow, config.zmodemBlockSize);
          trace(`offer accepted (ZRPOS), sending data in ${CHUNK}-byte subpackets`);

          // zmodem.js's streaming sender registers no header handler for the data
          // phase at all, and `_consume_header` reads `_next_header_handler[NAME]`
          // straight off a null — so *any* header the receiver sends killed the
          // upload with "Cannot read properties of null (reading 'ZRPOS')". It
          // also nulls the field on **every** dispatch, so installing a handler
          // once is not enough: a receiver that repeats itself (lrzsz retries its
          // ZRPOS) walks into the same null on the second one. The handler below
          // therefore re-arms itself and stays in place for the whole send.
          //
          // ZRPOS is the receiver asking us to resume from an offset — ZMODEM's
          // own repair mechanism, and one we can honour, since the file is
          // already in memory and there is nothing to re-read.
          const rewind = { offset: null, gaveUp: false };
          // Set while waiting for the ZEOF acknowledgement; the same two headers
          // mean different things then, so the handler needs to know which.
          let ackWaiter = null;
          function settleAck(value) {
            if (!ackWaiter) return;
            const resolve = ackWaiter;
            ackWaiter = null;
            resolve(value);
          }
          function armSendHandler() {
            // A windowed send installs and awaits its own handler; leave it be.
            if (zsession._windowDrain) return;
            zsession._next_header_handler = {
              ZRPOS: hdr => {
                rewind.offset = hdr.get_offset();
                armSendHandler();
                settleAck(false);
              },
              ZRINIT: () => {
                // Answering the ZEOF it acknowledges the file; at any other time
                // the receiver has re-announced itself, i.e. given up on us.
                if (!ackWaiter) rewind.gaveUp = true;
                armSendHandler();
                settleAck(true);
              },
            };
          }
          // Immediately: send_offer's own ZRPOS handler has just fired and left
          // the field null, and a repeat would land in that gap.
          armSendHandler();

          // A rewind can be asked for at either of two moments, and both have to
          // be handled or the upload dies: mid-stream, and — because a streaming
          // sender empties the whole file onto the wire long before a complaint
          // can travel back — during the wait for the ZEOF acknowledgement.
          // Measured against a real `rz` with one byte corrupted 100 KB in: the
          // ZRPOS arrived only after the last byte had gone out.
          let off = 0;
          let rewinds = 0;
          // Highest offset the receiver has ever rewound *to*, i.e. how much of
          // the file it has actually taken. Stuck at 0 means nothing has landed.
          let accepted = 0;

          // Returns the offset to resume from, or null when there is nothing to
          // rewind to. Throws once resending has stopped being worth trying.
          function takeRewind() {
            if (rewind.gaveUp) {
              throw new Error('receiver re-announced itself mid-file - it gave up on this transfer');
            }
            const target = rewind.offset;
            if (target === null) return null;
            rewind.offset = null;
            if (target > accepted) accepted = target;
            // A receiver that has never rewound past 0 has never accepted a data
            // byte, and one more full re-upload will not change that — which on a
            // large file is an expensive way to learn nothing. Allow exactly one
            // retry there, spent on the block-size fallback below.
            const budget = accepted > 0 ? MAX_REWINDS : 1;
            if (++rewinds > budget) {
              throw new Error(
                `receiver rewound us ${rewinds} times (last to offset ${target}) - ` + (accepted > 0
                  ? 'the data is not arriving intact, so resending will not fix it'
                  : 'it has never accepted a single data byte. The commonest cause by ' +
                    `far is the board refusing the file on size - this one is ${buf.length} ` +
                    'bytes, and a BBS states its per-file limit on the upload menu (often ' +
                    'a few hundred KB). A board that will not take the file answers ZRPOS 0 ' +
                    'and keeps answering it, which looks exactly like this. Check that ' +
                    'limit first. If the file is within it, the next suspect is a byte the ' +
                    'ASCII of the offer never contains: 0xFF, which ZMODEM cannot escape ' +
                    'in-band - see "outbound_iac" in the gateway log and the telnet: ' +
                    'setting on this board\'s phonebook entry.')
              );
            }
            if (CHUNK !== SUBPACKET_1K) {
              // The one cause of a rejected frame this gateway has ever pinned
              // down is an over-long subpacket, so spend the first rewind on
              // that before concluding the link itself is bad.
              CHUNK = SUBPACKET_1K;
              trace(`receiver rewound to offset ${target} - retrying from there in ` +
                `${CHUNK}-byte subpackets`);
            } else {
              trace(`receiver rewound to offset ${target} - resending from there`);
            }
            // The next subpacket has to announce where it now starts, so make
            // zmodem.js emit a fresh ZDATA carrying the rewound offset. `end()`
            // clears _sending_file and zeroes the offset, so a rewind out of the
            // ZEOF wait has to put both back.
            zsession._sending_file = true;
            zsession._file_offset = target;
            zsession._sent_ZDATA = false;
            currentXferBytes = target;
            return target;
          }

          // Sends the closing frame and ZEOF, then waits — for the ZRINIT that
          // acknowledges the file, or for the ZRPOS that says start again. zmodem
          // .js registers only the former (`_prepare_to_receive_ZRINIT`), so the
          // latter reached `_consume_header` with no handler and threw.
          function endFile(waitSecs) {
            return new Promise((resolve, reject) => {
              armTimeout(waitSecs); // the clock starts now, not at the last slice
              ackWaiter = resolve;
              // _end_file installs its own {ZRINIT} handler before sending the
              // ZEOF, so ours goes back on top of it — after which the library's
              // promise never resolves, because we took the handler that would
              // have resolved it. Both are wired up regardless: a windowed send
              // owns its handler and armSendHandler leaves it alone, so there the
              // library's promise is the only one that can settle this.
              xfer.end().then(() => settleAck(true), reject);
              armSendHandler();
            });
          }

          for (;;) {
            while (off < buf.length) {
              const slice = buf.subarray(off, Math.min(off + CHUNK, buf.length));
              // Make the final slice an acknowledged one, so a windowed receiver
              // has to tell us how much it really has before we send ZEOF.
              zsession._ackFinalPiece = off + CHUNK >= buf.length;
              await sendChunk(zsession, xfer, slice);
              await drainWS(zsession);
              off += slice.length;
              currentXferBytes = off;
              const elapsed = (performance.now() - currentXferStart) / 1000;
              const cps = elapsed > 0 ? currentXferBytes / elapsed : 0;
              window.ZmodemUI.updateXfer({ bytes: currentXferBytes, cps });
              armTimeout();

              const resumeAt = takeRewind();
              if (resumeAt !== null) off = resumeAt;
            }
            // ZEOF is queued behind every byte above it, so empty the socket
            // before we start counting the receiver's response time against the
            // idle timeout — otherwise a large file times out on its own backlog.
            await drainWS(zsession, 0);

            // Only meaningful for a windowed receiver; a streaming one never
            // acknowledges anything mid-file and there is nothing to check.
            if (receiverWindow > 0) {
              const confirmed = zsession._ackedOffset;
              if (!zsession._ackCount) {
                throw new Error('receiver never acknowledged any of the data');
              }
              // Only a receiver that reported a real offset can contradict us.
              if (confirmed > 0 && confirmed !== buf.length) {
                throw new Error(
                  `receiver confirmed ${confirmed} of ${buf.length} bytes — the file did not arrive intact`
                );
              }
              trace(confirmed > 0
                ? `receiver confirmed all ${confirmed} bytes`
                : `receiver acknowledged the closing frame (${buf.length} bytes)`);
            }

            // The receiver cannot answer the ZEOF until it has chewed through
            // everything still buffered between us and it, and the browser's own
            // socket draining says nothing about how much that is — nginx,
            // Cloudflare and the gateway all sit in between and each holds its
            // own queue. How long this file took to push is the best measure
            // available of how long the far end can still be busy with it, so
            // the acknowledgement gets at least that long. On the configured 30s
            // a 116 MB upload dies here with the file still landing.
            const dataSecs = Math.ceil((performance.now() - currentXferStart) / 1000);
            const ackWait = Math.min(600, Math.max(config.zmodemTimeoutSec, dataSecs));
            stage = 'waiting for ZEOF acknowledgement';
            trace(`all ${buf.length} bytes sent, waiting up to ${ackWait}s for the receiver`);
            if (await endFile(ackWait)) break;

            stage = 'streaming data';
            const resumeAt = takeRewind();
            // finish(false) only happens with an offset recorded, so this cannot
            // spin: takeRewind either returns one or throws.
            if (resumeAt === null) throw new Error('receiver rejected the file without saying where to resume');
            off = resumeAt;
          }
          trace('receiver acknowledged the file');
          window.ZmodemUI.endXfer({ status: 'done' });
        }
        disarmTimeout();
        stage = 'closing session (ZFIN)';
        try { await zsession.close(); } catch (e) { /* nop */ }
        trace('session closed cleanly');
      })().catch(err => {
        disarmTimeout();
        console.error('ZMODEM send error', err);
        trace(`FAILED while ${stage}: ${err && err.message ? err.message : err}`);
        // The idle timer already ended the strip as TIMEOUT, which is the more
        // specific label; relabelling it ABORTED would lose that.
        if (!(err && err.alreadyReported)) window.ZmodemUI.endXfer({ status: 'aborted' });
        try { zsession.abort(); } catch (e) { /* nop */ }
      });
    }

    return {
      consume(bytes) {
        const buf = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
        // Split on every chunk, session or not. A session can end part-way
        // through a chunk and the bytes behind it may open the next one, so
        // feed() has to re-read the session state at each header boundary —
        // which only works if headers arrive as slices of their own.
        clearTailFlush();
        let input = buf;
        if (headerTail) {
          input = new Uint8Array(headerTail.length + buf.length);
          input.set(headerTail, 0);
          input.set(buf, headerTail.length);
          headerTail = null;
        }

        const { slices, tail } = splitAtHeaderBoundaries(input);
        for (const slice of slices) feed(slice);

        headerTail = tail;
        scheduleTailFlush();
      },
      // Also active while the file picker is open: keystrokes typed then would
      // interleave with the offer we are about to send.
      isActive() { return session !== null || pendingSend !== null; },
    };
  }

  window.ZmodemSentry = { createZmodemBridge, splitAtHeaderBoundaries, chooseBlockSize };
})();
