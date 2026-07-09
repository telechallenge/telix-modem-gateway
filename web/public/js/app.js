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

  function checkModemState(text) {
    if (/CONNECT\b/.test(text)) {
      setConnected(true);
      ModemAudio.onConnect();
    } else if (/BUSY/.test(text)) {
      setConnected(false);
      ModemAudio.onBusy();
    } else if (/NO CARRIER|NO ANSWER|NO DIALTONE/.test(text)) {
      setConnected(false);
      ModemAudio.onFailure();
    } else if (/RING/.test(text)) {
      ledOn('oh');
      ModemAudio.onRing();
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
    if (ws.readyState === WebSocket.OPEN) {
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
