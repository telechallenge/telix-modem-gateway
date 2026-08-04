document.addEventListener('DOMContentLoaded', () => {
  // CGA/VGA 16-color palette
  const CGA_PALETTE = {
    black:         '#000000',
    red:           '#AA0000',
    green:         '#00AA00',
    yellow:        '#AA5500',
    blue:          '#0000AA',
    magenta:       '#AA00AA',
    cyan:          '#00AAAA',
    white:         '#AAAAAA',
    brightBlack:   '#555555',
    brightRed:     '#FF5555',
    brightGreen:   '#55FF55',
    brightYellow:  '#FFFF55',
    brightBlue:    '#5555FF',
    brightMagenta: '#FF55FF',
    brightCyan:    '#55FFFF',
    brightWhite:   '#FFFFFF',
    background:    '#000000',
    foreground:    '#AAAAAA',
    cursor:        '#AAAAAA',
  };

  const term = new Terminal({
    fontFamily: "'PxPlus IBM VGA 8x16', monospace",
    fontSize: 16,
    cols: 80,
    rows: 24,
    cursorBlink: true,
    cursorStyle: 'block',
    theme: CGA_PALETTE,
    allowTransparency: false,
    scrollback: 1000,
    convertEol: false,
  });

  // Try WebGL renderer, fall back to canvas
  try {
    const webglAddon = new WebglAddon.WebglAddon();
    webglAddon.onContextLoss(() => {
      webglAddon.dispose();
    });
    term.loadAddon(webglAddon);
  } catch (e) {
    console.warn('WebGL renderer not available, using canvas');
  }

  term.open(document.getElementById('terminal'));

  // --- Fluid sizing ---
  // The grid stays 80x24 — BBS ANSI art is authored for 80 columns — so filling
  // the window means growing the cell, not adding columns. This owns the scale
  // for the whole assembly: `--crt-scale` sizes the CRT and modem chrome (see
  // terminal.css) and xterm's font size moves with it, so glyphs are rasterized
  // at their final size rather than upscaled from a 16px canvas.
  const crtContainer = document.querySelector('.crt-container');
  const BASE_FONT_SIZE = 16;
  const VIEWPORT_FILL = 0.9;
  const MIN_FONT_SIZE = 6; // below this the window is too small to be usable anyway
  let cellFontSize = BASE_FONT_SIZE;
  let fitFrame = 0;

  function applyFontSize(size) {
    cellFontSize = size;
    crtContainer.style.setProperty('--crt-scale', String(size / BASE_FONT_SIZE));
    term.options.fontSize = size;
  }

  // Measure, correct, repeat. Solving for the size outright would need xterm's
  // cell metrics, and measuring is both simpler and free of private APIs. Passes
  // are spread across animation frames because xterm resizes its screen element
  // on its own schedule after a font-size change.
  //
  // Two rules keep this from oscillating. Steps are whole pixels of font size:
  // the cell metrics xterm derives are integers, so across 24 rows the assembly
  // height moves in ~24px jumps, and a fractional step can leave it a hair over
  // the target with no step small enough to correct — the loop then grinds
  // without converging. And growth stops once a pass has shrunk (`lastDir`),
  // since that means we have already straddled a step boundary and come down to
  // the side that fits. Landing up to one step under 90% is the cost of that,
  // and a whole-pixel size renders the bitmap font cleanly besides.
  function fitCrt(passes, shrinkOnly, lastDir) {
    const width = crtContainer.offsetWidth;
    const height = crtContainer.offsetHeight;
    if (!width || !height || passes <= 0) return;

    const ratio = Math.min(
      (window.innerWidth * VIEWPORT_FILL) / width,
      (window.innerHeight * VIEWPORT_FILL) / height,
    );

    let next = Math.round(cellFontSize * ratio);
    if (next === cellFontSize) {
      if (ratio >= 1) return; // fits, and there is no whole step left to grow
      next = cellFontSize - 1; // over by less than a step: come down one
    }
    next = Math.max(MIN_FONT_SIZE, next);

    const dir = next > cellFontSize ? 1 : -1;
    if (dir === 1 && (shrinkOnly || lastDir === -1)) return;
    if (next === cellFontSize) return;

    applyFontSize(next);
    scheduleFit(passes - 1, shrinkOnly, dir);
  }

  function scheduleFit(passes = 8, shrinkOnly = false, lastDir = 0) {
    cancelAnimationFrame(fitFrame);
    fitFrame = requestAnimationFrame(() => fitCrt(passes, shrinkOnly, lastDir));
  }

  scheduleFit();
  window.addEventListener('resize', () => scheduleFit());
  // The bitmap font lands after first paint and changes the cell size with it.
  if (document.fonts) document.fonts.ready.then(() => scheduleFit());
  if (window.ResizeObserver) {
    // Content can widen the assembly mid-session — the transfer strip stretches
    // the modem front. Only react when that actually pushes past the target, and
    // only ever shrink: growing here would change the very size being observed.
    new ResizeObserver(() => {
      const overflows = crtContainer.offsetWidth > window.innerWidth * VIEWPORT_FILL
        || crtContainer.offsetHeight > window.innerHeight * VIEWPORT_FILL;
      if (overflows) scheduleFit(2, true);
    }).observe(crtContainer);
  }

  // --- Modem LED panel ---
  const leds = {};
  document.querySelectorAll('.modem-led').forEach(el => {
    leds[el.dataset.led] = el;
  });

  function ledOn(name) {
    if (leds[name]) leds[name].classList.add('on');
  }

  function ledOff(name) {
    if (leds[name]) leds[name].classList.remove('on', 'active');
  }

  function flashLed(name) {
    const el = leds[name];
    if (!el) return;
    el.classList.add('active');
    clearTimeout(el._t);
    el._t = setTimeout(() => el.classList.remove('active'), 80);
  }

  let modemConnected = false;

  function setConnected(on) {
    modemConnected = on;
    ['hs', 'cd', 'oh', 'cs', 'arq'].forEach(n => on ? ledOn(n) : ledOff(n));
  }

  function applyResultCode(code) {
    // Once in data mode the only code the modem can still emit is a carrier
    // drop. Everything else on the wire is BBS output, which must never be
    // able to restart the dial audio mid-session.
    if (modemConnected && code !== 'NO CARRIER') return;

    if (code === 'CONNECT') {
      setConnected(true);
      ModemAudio.onConnect();
    } else if (code === 'BUSY') {
      setConnected(false);
      ModemAudio.onBusy();
    } else if (code === 'RING') {
      ledOn('oh');
      ModemAudio.onRing();
    } else {
      // NO CARRIER / NO ANSWER / NO DIALTONE
      setConnected(false);
      ModemAudio.onFailure();
    }
  }

  function checkModemState(text) {
    // A chunk can carry more than one code (RING then CONNECT); apply them in
    // arrival order so the last one wins.
    for (const code of window.ModemState.parseResultCodes(text)) {
      applyResultCode(code);
    }
  }

  // Modem power on
  ledOn('mr');

  // Mute button
  const muteBtn = document.getElementById('muteBtn');
  const muteIcon = document.getElementById('muteIcon');
  function updateMuteIcon() {
    muteIcon.textContent = ModemAudio.isMuted() ? '\u{1F507}' : '\u{1F50A}';
  }
  updateMuteIcon();
  muteBtn.addEventListener('click', () => {
    ModemAudio.setMuted(!ModemAudio.isMuted());
    updateMuteIcon();
  });

  // WebSocket connection
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  ws.binaryType = 'arraybuffer';

  let bridge = null;
  let config = { maxUploadBytes: 1073741824, zmodemTimeoutSec: 30 };

  fetch('/config.json')
    .then(r => r.json())
    .then(c => { config = c; })
    .catch(() => { /* use defaults */ });

  ws.onopen = () => {
    ledOn('tr');
    ledOn('aa');
    sendResize();
  };

  ws.onmessage = (event) => {
    if (!(event.data instanceof ArrayBuffer)) {
      // ws.binaryType is 'arraybuffer' (see above); anything else means a bug
      // upstream or a broken environment. Loud failure beats silent reordering.
      console.error('WebSocket delivered non-binary frame; dropping', typeof event.data);
      return;
    }
    if (!bridge) {
      bridge = window.ZmodemSentry.createZmodemBridge({
        ws, term, config, checkModemState, flashLed,
      });
    }
    bridge.consume(new Uint8Array(event.data));
  };

  ws.onclose = () => {
    term.write('\r\n\x1b[1;31m[Connection closed]\x1b[0m\r\n');
    ledOff('tr');
    ledOff('aa');
    setConnected(false);
    ModemAudio.stopAll();
  };

  ws.onerror = () => {
    term.write('\r\n\x1b[1;31m[Connection error]\x1b[0m\r\n');
  };

  // Send keystrokes to server
  let cmdBuffer = '';
  term.onData((data) => {
    if (ws.readyState !== WebSocket.OPEN) return;

    // During an active ZMODEM session, only pass through Ctrl-X (CAN, 0x18)
    // so the user can still trigger the standard ZMODEM cancel sequence
    // (Ctrl-X × 5). All other keystrokes would interleave with the transfer
    // stream and corrupt it.
    if (bridge && bridge.isActive()) {
      const canOnly = [...data].filter(ch => ch === '\x18').join('');
      if (canOnly.length > 0) {
        ws.send(canOnly);
        flashLed('sd');
      }
      return;
    }

    ws.send(data);
    flashLed('sd');
    ModemAudio.initOnGesture();

    // Buffer keystrokes to detect ATD commands
    for (const ch of data) {
      if (ch === '\r' || ch === '\n') {
        const match = cmdBuffer.match(/^ATD[TP]([0-9\-\(\)\.\*# ]+)$/i);
        if (match) ModemAudio.dialNumber(match[1].replace(/[-().\s]/g, ''));
        cmdBuffer = '';
      } else if (ch === '\x08' || ch === '\x7F') {
        cmdBuffer = cmdBuffer.slice(0, -1);
      } else {
        cmdBuffer += ch;
      }
    }
  });

  // Send terminal dimensions to server
  function sendResize() {
    if (ws.readyState === WebSocket.OPEN) {
      const cols = term.cols;
      const rows = term.rows;
      const buf = new Uint8Array(5);
      buf[0] = 0x00; // resize marker
      buf[1] = (cols >> 8) & 0xFF;
      buf[2] = cols & 0xFF;
      buf[3] = (rows >> 8) & 0xFF;
      buf[4] = rows & 0xFF;
      ws.send(buf.buffer);
    }
  }
});
