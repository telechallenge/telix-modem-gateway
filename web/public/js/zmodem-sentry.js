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

  // zmodem.js only supports receivers that advertise a zero buffer size, i.e.
  // pure streaming. DSZ/GSZ (and other DOS ZMODEM implementations) advertise a
  // non-zero receive buffer in their ZRINIT, which makes _consume_ZRINIT throw
  // "Buffer size (...) is unsupported". The Sentry swallows that throw, so an
  // upload to such a BBS never even opens the file picker — the ZRINIT just
  // echoes to the terminal and `rz` retransmits forever.
  //
  // The buffer size is advisory windowing; over TCP the sender can stream and
  // the receiver copes. So we rewrite an inbound ZRINIT to advertise buffer 0,
  // preserving its capability flags, and fix up the CRC. Keeping this in our
  // code leaves the vendored library untouched.
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

  function neutralizeZrinitBuffer(chunk) {
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
      if (isZrinitHeader(chunk)) chunk = neutralizeZrinitBuffer(chunk);
      if (chunk.length > 0) sentry.consume(chunk);
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
        return handleSend(confirmSession(detection), files);
      }).catch(err => {
        pendingSend = null;
        uploadPrompt = null;
        disarmTimeout();
        console.error('ZMODEM upload error', err);
        window.ZmodemUI.endXfer({ status: 'aborted' });
      });
    }

    function handleSend(zsession, files) {
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
          const xfer = await zsession.send_offer(offer);
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
        disarmTimeout();
        try { await zsession.close(); } catch (e) { /* nop */ }
      })().catch(err => {
        disarmTimeout();
        console.error('ZMODEM send error', err);
        window.ZmodemUI.endXfer({ status: 'aborted' });
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

  window.ZmodemSentry = { createZmodemBridge, splitAtHeaderBoundaries };
})();
