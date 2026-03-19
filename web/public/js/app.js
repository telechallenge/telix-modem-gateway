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
    } else if (/NO CARRIER|BUSY|NO ANSWER|NO DIALTONE/.test(text)) {
      setConnected(false);
    } else if (/RING/.test(text)) {
      ledOn('oh');
    }
  }

  // Modem power on
  ledOn('mr');

  // WebSocket connection
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    ledOn('tr');
    ledOn('aa');
    sendResize();
  };

  ws.onmessage = (event) => {
    if (typeof event.data === 'string') {
      term.write(event.data);
      checkModemState(event.data);
    } else {
      const bytes = new Uint8Array(event.data);
      term.write(bytes);
      if (bytes.length < 200) {
        checkModemState(new TextDecoder().decode(bytes));
      }
    }
    flashLed('rd');
  };

  ws.onclose = () => {
    term.write('\r\n\x1b[1;31m[Connection closed]\x1b[0m\r\n');
    ledOff('tr');
    ledOff('aa');
    setConnected(false);
  };

  ws.onerror = () => {
    term.write('\r\n\x1b[1;31m[Connection error]\x1b[0m\r\n');
  };

  // Send keystrokes to server
  term.onData((data) => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(data);
      flashLed('sd');
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
