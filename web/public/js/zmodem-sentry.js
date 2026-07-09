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

      zsession.on('offer', xfer => {
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

    function handleSend(zsession) {
      window.ZmodemUI.promptUpload(config.maxUploadBytes).then(async files => {
        if (files.length === 0) {
          disarmTimeout();
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
        disarmTimeout();
        try { await zsession.close(); } catch (e) { /* nop */ }
      }).catch(err => {
        disarmTimeout();
        console.error('ZMODEM send error', err);
        window.ZmodemUI.endXfer({ status: 'aborted' });
        try { zsession.abort(); } catch (e) { /* nop */ }
      });
    }

    return {
      consume(bytes) { sentry.consume(bytes); },
      isActive() { return session !== null; },
    };
  }

  window.ZmodemSentry = { createZmodemBridge };
})();
