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

// IAC-escape bytes for outbound telnet: 0xFF must be doubled per RFC 854.
// Runs on all browser→Go bytes (not just during ZMODEM) so we can never send an
// accidental unescaped IAC. Cost is O(n); zero allocation when no 0xFF present.
function escapeIAC(buf) {
  let hits = 0;
  for (let i = 0; i < buf.length; i++) if (buf[i] === IAC) hits++;
  if (hits === 0) return buf;
  const out = Buffer.allocUnsafe(buf.length + hits);
  let j = 0;
  for (let i = 0; i < buf.length; i++) {
    out[j++] = buf[i];
    if (buf[i] === IAC) out[j++] = IAC;
  }
  return out;
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

  // Data from Telix → browser: raw binary passthrough (Go server already
  // handles telnet negotiation, so data is clean application bytes).
  tcp.on('data', (data) => {
    if (ws.readyState === ws.OPEN) {
      ws.send(data, { binary: true });
    }
  });

  // Data from browser → Telix
  ws.on('message', (data, isBinary) => {
    if (isBinary && data.length >= 5 && data[0] === 0x00) {
      // Resize message: 0x00 + cols(uint16 BE) + rows(uint16 BE)
      const cols = (data[1] << 8) | data[2];
      const rows = (data[3] << 8) | data[4];
      const naws = Buffer.from([
        IAC, SB, NAWS,
        (cols >> 8) & 0xFF, cols & 0xFF,
        (rows >> 8) & 0xFF, rows & 0xFF,
        IAC, SE,
      ]);
      tcp.write(naws); // NAWS is proxy-constructed and correctly formed; do not re-escape.
    } else {
      tcp.write(escapeIAC(Buffer.isBuffer(data) ? data : Buffer.from(data)));
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
  const addr = server.address();
  const listeningPort = typeof addr === 'object' && addr ? addr.port : PORT;
  console.log(`Telix web terminal listening on http://0.0.0.0:${listeningPort}`);
  console.log(`Proxying to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
});

module.exports = server;
