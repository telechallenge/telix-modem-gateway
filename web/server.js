const express = require('express');
const http = require('http');
const net = require('net');
const { WebSocketServer } = require('ws');
const path = require('path');

const TELIX_HOST = process.env.TELIX_HOST || 'localhost';
const TELIX_PORT = parseInt(process.env.TELIX_PORT || '2323', 10);
const PORT = parseInt(process.env.PORT || '3000', 10);

// --- Telnet protocol constants (only what's needed for NAWS generation) ---
const IAC  = 255;
const SB   = 250;
const SE   = 240;
const NAWS = 31;

// --- CP437 to Unicode mapping ---
// Control chars and ASCII printable pass through; this table covers 0x00-0x1F
// and 0x7F-0xFF CP437 graphic characters mapped to Unicode code points.
const CP437_TO_UNICODE = new Uint32Array(256);

// Initialize: ASCII printable range maps to itself
for (let i = 0; i < 256; i++) {
  CP437_TO_UNICODE[i] = i;
}

// CP437 control-range graphic characters (0x01-0x1F, excluding pass-through controls)
const cp437Low = {
  0x01: 0x263A, // ☺
  0x02: 0x263B, // ☻
  0x03: 0x2665, // ♥
  0x04: 0x2666, // ♦
  0x05: 0x2663, // ♣
  0x06: 0x2660, // ♠
  0x0B: 0x2642, // ♂
  0x0C: 0x2640, // ♀
  0x0E: 0x266B, // ♫
  0x0F: 0x263C, // ☼
  0x10: 0x25BA, // ►
  0x11: 0x25C4, // ◄
  0x12: 0x2195, // ↕
  0x13: 0x203C, // ‼
  0x14: 0x00B6, // ¶
  0x15: 0x00A7, // §
  0x16: 0x25AC, // ▬
  0x17: 0x21A8, // ↨
  0x18: 0x2191, // ↑
  0x19: 0x2193, // ↓
  0x1A: 0x2192, // →
  0x1C: 0x221F, // ∟
  0x1D: 0x2194, // ↔
  0x1E: 0x25B2, // ▲
  0x1F: 0x25BC, // ▼
};

for (const [byte, cp] of Object.entries(cp437Low)) {
  CP437_TO_UNICODE[parseInt(byte)] = cp;
}

// 0x7F = house (⌂)
CP437_TO_UNICODE[0x7F] = 0x2302;

// CP437 high bytes (0x80-0xFF)
const cp437High = [
  0x00C7, 0x00FC, 0x00E9, 0x00E2, 0x00E4, 0x00E0, 0x00E5, 0x00E7, // 80-87
  0x00EA, 0x00EB, 0x00E8, 0x00EF, 0x00EE, 0x00EC, 0x00C4, 0x00C5, // 88-8F
  0x00C9, 0x00E6, 0x00C6, 0x00F4, 0x00F6, 0x00F2, 0x00FB, 0x00F9, // 90-97
  0x00FF, 0x00D6, 0x00DC, 0x00A2, 0x00A3, 0x00A5, 0x20A7, 0x0192, // 98-9F
  0x00E1, 0x00ED, 0x00F3, 0x00FA, 0x00F1, 0x00D1, 0x00AA, 0x00BA, // A0-A7
  0x00BF, 0x2310, 0x00AC, 0x00BD, 0x00BC, 0x00A1, 0x00AB, 0x00BB, // A8-AF
  0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556, // B0-B7
  0x2555, 0x2563, 0x2551, 0x2557, 0x255D, 0x255C, 0x255B, 0x2510, // B8-BF
  0x2514, 0x2534, 0x252C, 0x251C, 0x2500, 0x253C, 0x255E, 0x255F, // C0-C7
  0x255A, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256C, 0x2567, // C8-CF
  0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256B, // D0-D7
  0x256A, 0x2518, 0x250C, 0x2588, 0x2584, 0x258C, 0x2590, 0x2580, // D8-DF
  0x03B1, 0x00DF, 0x0393, 0x03C0, 0x03A3, 0x03C3, 0x00B5, 0x03C4, // E0-E7
  0x03A6, 0x0398, 0x03A9, 0x03B4, 0x221E, 0x03C6, 0x03B5, 0x2229, // E8-EF
  0x2261, 0x00B1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00F7, 0x2248, // F0-F7
  0x00B0, 0x2219, 0x00B7, 0x221A, 0x207F, 0x00B2, 0x25A0, 0x00A0, // F8-FF
];

for (let i = 0; i < cp437High.length; i++) {
  CP437_TO_UNICODE[0x80 + i] = cp437High[i];
}

// Pass-through control characters (these should NOT be converted to CP437 graphics)
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

// --- Per-IP connection limiting ---
const MAX_WS_PER_IP = parseInt(process.env.MAX_WS_PER_IP || '5', 10);
const ipConnections = new Map(); // ip -> count

function getClientIP(req) {
  // Trust X-Forwarded-For only from reverse proxies
  const forwarded = req.headers['x-forwarded-for'];
  if (forwarded) {
    return forwarded.split(',')[0].trim();
  }
  return req.socket.remoteAddress || 'unknown';
}

// --- Express + WebSocket server ---
const app = express();
app.use(express.static(path.join(__dirname, 'public')));

const server = http.createServer(app);
const wss = new WebSocketServer({ server, path: '/ws' });

wss.on('connection', (ws, req) => {
  const clientIP = getClientIP(req);
  const current = ipConnections.get(clientIP) || 0;

  if (current >= MAX_WS_PER_IP) {
    console.warn(`WebSocket rejected: ${clientIP} (${current}/${MAX_WS_PER_IP} connections)`);
    ws.close(1008, 'Too many connections');
    return;
  }

  ipConnections.set(clientIP, current + 1);
  console.log(`WebSocket client connected (${clientIP}, ${current + 1}/${MAX_WS_PER_IP})`);

  const tcp = net.createConnection({ host: TELIX_HOST, port: TELIX_PORT });

  tcp.on('connect', () => {
    console.log(`Connected to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
  });

  // Data from Telix → browser: direct CP437→UTF-8 passthrough (Go server
  // already handles telnet negotiation, so data is clean application bytes).
  tcp.on('data', (data) => {
    if (ws.readyState === ws.OPEN) {
      ws.send(cp437ToUtf8(data));
    }
  });

  // Data from browser → Telix
  ws.on('message', (data, isBinary) => {
    if (isBinary && data.length >= 3 && data[0] === 0x00) {
      // Resize message: 0x00 + cols(uint16 BE) + rows(uint16 BE)
      const cols = (data[1] << 8) | data[2];
      const rows = (data[3] << 8) | data[4];
      // Send NAWS subnegotiation to Telix
      const naws = Buffer.from([
        IAC, SB, NAWS,
        (cols >> 8) & 0xFF, cols & 0xFF,
        (rows >> 8) & 0xFF, rows & 0xFF,
        IAC, SE,
      ]);
      tcp.write(naws);
    } else {
      // Regular keystrokes — pass through as raw bytes
      tcp.write(data);
    }
  });

  // Consolidated teardown — ensures IP counter is always decremented
  // and both sockets are cleaned up regardless of which side closes first.
  let cleaned = false;
  function cleanup() {
    if (cleaned) return;
    cleaned = true;
    tcp.destroy();
    if (ws.readyState === ws.OPEN || ws.readyState === ws.CONNECTING) {
      ws.close();
    }
    const count = (ipConnections.get(clientIP) || 1) - 1;
    if (count <= 0) {
      ipConnections.delete(clientIP);
    } else {
      ipConnections.set(clientIP, count);
    }
  }

  tcp.on('error', (err) => {
    console.error(`TCP error (${clientIP}):`, err.message);
    cleanup();
  });

  tcp.on('close', () => {
    console.log(`TCP connection closed (${clientIP})`);
    cleanup();
  });

  ws.on('close', () => {
    console.log(`WebSocket client disconnected (${clientIP})`);
    cleanup();
  });

  ws.on('error', (err) => {
    console.error(`WebSocket error (${clientIP}):`, err.message);
    cleanup();
  });
});

server.listen(PORT, () => {
  console.log(`Telix web client listening on http://localhost:${PORT}`);
  console.log(`Proxying to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
});
