const express = require('express');
const http = require('http');
const net = require('net');
const { WebSocketServer } = require('ws');
const path = require('path');
const fs = require('fs');
const { computeAssetVersion, stampAssetUrls } = require('./asset-version');
const { Metrics, CONTENT_TYPE: METRICS_CONTENT_TYPE } = require('./metrics');

const metrics = new Metrics();

const TELIX_HOST = process.env.TELIX_HOST || 'localhost';
const TELIX_PORT = parseInt(process.env.TELIX_PORT || '2323', 10);
const PORT = parseInt(process.env.PORT || '3000', 10);
const MAX_UPLOAD_BYTES = parseInt(process.env.MAX_UPLOAD_BYTES || String(1024 * 1024 * 1024), 10);
const ZMODEM_TIMEOUT_SEC = parseInt(process.env.ZMODEM_TIMEOUT_SEC || '30', 10);
// Outbound ZMODEM data subpacket size. 'auto' reads it off the receiver's
// ZRINIT (see chooseBlockSize in public/js/zmodem-sentry.js): 1024 for a
// receiver that advertises a window, 8192 — "ZMODEM-8k", what ENiGMA½'s
// sexyz/lrzsz receivers expect — for one that streams. Only those two sizes are
// meaningful, so anything else falls back to 'auto' rather than putting an
// arbitrary number on the wire.
const ZMODEM_BLOCK_SIZE = [1024, 8192].includes(parseInt(process.env.ZMODEM_BLOCK_SIZE, 10))
  ? parseInt(process.env.ZMODEM_BLOCK_SIZE, 10)
  : 0; // 0 = auto

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

// The client identity behind nginx, used for MAX_WS_PER_IP and for the
// connection log.
//
// X-Real-IP first, and that ordering is the whole point. nginx *sets*
// X-Real-IP from $remote_addr (post-real_ip, so the CF-Connecting-IP that
// Cloudflare vouched for), overwriting any copy the caller sent — whereas
// X-Forwarded-For is $proxy_add_x_forwarded_for, which *appends* to the
// caller's own header. So the first XFF entry is chosen by the caller: reading
// it handed every forged value its own fresh MAX_WS_PER_IP budget, which is no
// limit at all. When falling back to XFF, take the last entry — the hop
// appended by the proxy nearest us, not the oldest claim in the chain.
//
// This leans on telix-web being unreachable except from nginx (it publishes no
// host port and sits on telix-net); exposing it directly would make both
// headers caller-controlled again.
function getClientIP(req) {
  const realIP = req.headers['x-real-ip'];
  if (realIP) return realIP.trim();

  const forwarded = req.headers['x-forwarded-for'];
  if (forwarded) {
    const hops = forwarded.split(',');
    return hops[hops.length - 1].trim();
  }
  return req.socket.remoteAddress || 'unknown';
}

// --- Asset versioning (cache-busting) ---
// Static JS/CSS is loaded with no version marker, so a browser (or the
// Cloudflare edge in front of the origin) will keep serving a cached copy long
// after a redeploy. That silently strands users on old code. We stamp a
// content hash onto each local asset URL in index.html and serve the HTML itself
// as no-cache, so a changed bundle always produces a fresh URL that no cache can
// satisfy from a stale copy.
const PUBLIC_DIR = path.join(__dirname, 'public');

// Hash of the bundle and the versioned index.html, computed once at startup —
// the files can't change under a running server.
let INDEX_HTML;
try {
  const version = computeAssetVersion(PUBLIC_DIR);
  INDEX_HTML = stampAssetUrls(fs.readFileSync(path.join(PUBLIC_DIR, 'index.html'), 'utf8'), version);
} catch (e) {
  INDEX_HTML = null; // fall back to static serving of the raw index.html
}

// --- Security headers ---
// Hand-rolled rather than pulled from helmet for the same reason metrics.js is:
// the image installs with `npm ci`, which aborts when package.json and the lock
// file disagree, so a new dependency means regenerating the lock. This is a
// fixed set of static headers — the part helmet earns its keep on (per-request
// nonces, CSP report parsing) is not in play here.
//
// The CSP is written against what the page actually loads:
//   script-src   xterm and its WebGL addon come from jsdelivr, pinned by SRI in
//                index.html. No inline script anywhere, so no 'unsafe-inline'
//                and no nonce plumbing.
//   style-src    'unsafe-inline' is unavoidable: xterm builds a <style> element
//                at runtime for its cell metrics and theme, and a library that
//                does not take a nonce cannot be covered any other way. It is a
//                far weaker grant than inline script.
//   img-src      data: for the moulded-plastic noise tile in terminal.css.
//   connect-src  'self' covers the same-origin WebSocket — ws:// and wss:// of
//                the page's own origin match 'self' in CSP3. Deliberately not
//                widened to bare `ws:`, which would allow any host.
//   font-src     the CP437 bitmap font is served from here, not a font CDN.
// Everything unnamed falls through to default-src 'self'; the four 'none'
// directives close off clickjacking, <base> hijacking, plugin content and
// form-based exfiltration, none of which this app has any use for.
const CDN_ORIGIN = 'https://cdn.jsdelivr.net';
const CSP = [
  "default-src 'self'",
  `script-src 'self' ${CDN_ORIGIN}`,
  `style-src 'self' 'unsafe-inline' ${CDN_ORIGIN}`,
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
].join('; ');

// A terminal needs none of these. Named explicitly rather than left to the
// defaults so a future embed cannot inherit them.
const PERMISSIONS_POLICY = [
  'accelerometer=()', 'camera=()', 'display-capture=()', 'geolocation=()',
  'gyroscope=()', 'magnetometer=()', 'microphone=()', 'midi=()',
  'payment=()', 'usb=()',
].join(', ');

const HSTS_MAX_AGE = parseInt(process.env.HSTS_MAX_AGE || '15552000', 10); // 180 days

// True when the browser reached us over TLS, directly or through the nginx /
// Cloudflare front (nginx.conf sets X-Forwarded-Proto $scheme). Same trust
// model as getClientIP: the header is only meaningful because a reverse proxy
// in front of this process sets it.
function isSecureRequest(req) {
  const forwarded = req.headers['x-forwarded-proto'];
  if (forwarded) return forwarded.split(',')[0].trim() === 'https';
  return Boolean(req.socket.encrypted);
}

function securityHeaders(req, res, next) {
  res.set('Content-Security-Policy', CSP);
  res.set('X-Content-Type-Options', 'nosniff');
  res.set('X-Frame-Options', 'DENY'); // frame-ancestors' predecessor, for old browsers
  res.set('Referrer-Policy', 'no-referrer'); // nothing here links out
  res.set('Permissions-Policy', PERMISSIONS_POLICY);
  res.set('Cross-Origin-Opener-Policy', 'same-origin');
  res.set('Cross-Origin-Resource-Policy', 'same-origin');
  // Only over TLS. Sent on a plain-HTTP response a browser ignores it anyway,
  // and promising HSTS for a deployment that has no TLS would lock users out.
  if (isSecureRequest(req)) {
    res.set('Strict-Transport-Security', `max-age=${HSTS_MAX_AGE}; includeSubDomains`);
  }
  next();
}

// --- Express + WebSocket server ---
const app = express();

app.disable('x-powered-by'); // no free version disclosure
app.use(securityHeaders);

// Serve the versioned HTML for the app entry points, always revalidated so the
// stamped asset URLs are never themselves cached stale.
app.get(['/', '/index.html'], (req, res, next) => {
  if (!INDEX_HTML) return next();
  res.set('Cache-Control', 'no-cache');
  res.type('html').send(INDEX_HTML);
});

app.use(express.static(PUBLIC_DIR));

app.get('/config.json', (req, res) => {
  res.json({
    maxUploadBytes: MAX_UPLOAD_BYTES,
    zmodemTimeoutSec: ZMODEM_TIMEOUT_SEC,
    zmodemBlockSize: ZMODEM_BLOCK_SIZE,
  });
});

// Prometheus scrape endpoint. Shares the app's port rather than taking a second
// listener: this server already speaks HTTP, and the host-side exposure is
// restricted by the port bindings in docker-compose.yml.
app.get('/metrics', (req, res) => {
  res.set('Content-Type', METRICS_CONTENT_TYPE);
  res.set('Cache-Control', 'no-store');
  res.send(metrics.render());
});

app.get('/healthz', (req, res) => res.type('text').send('ok\n'));

const server = http.createServer(app);
const wss = new WebSocketServer({ server, path: '/ws' });

wss.on('connection', (ws, req) => {
  const clientIP = getClientIP(req);
  const current = ipConnections.get(clientIP) || 0;

  if (current >= MAX_WS_PER_IP) {
    console.warn(`WebSocket rejected: ${clientIP} (${current}/${MAX_WS_PER_IP} connections)`);
    metrics.wsRejected();
    ws.close(1008, 'Too many connections');
    return;
  }

  ipConnections.set(clientIP, current + 1);
  metrics.wsConnected();
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
      metrics.bytesToBrowser(data.length);
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
      const out = escapeIAC(Buffer.isBuffer(data) ? data : Buffer.from(data));
      metrics.bytesToGateway(out.length);
      // Backpressure. net.Socket#write buffers in the heap and returns false
      // when it is over its high-water mark; ignoring that meant a ZMODEM upload
      // simply relocated from the browser's socket buffer into this process,
      // which is no better — the browser sees its own buffer drain, believes the
      // file has landed and starts its idle timer while the bytes are still
      // sitting here. Stop reading the WebSocket until the gateway has taken
      // what we already have, so the backlog stays where TCP can signal it.
      if (!tcp.write(out)) {
        ws.pause();
        tcp.once('drain', () => ws.resume());
      }
    }
  });

  // Consolidated teardown — ensures IP counter is always decremented
  // and both sockets are cleaned up regardless of which side closes first.
  let cleaned = false;
  function cleanup() {
    if (cleaned) return;
    cleaned = true;
    metrics.wsDisconnected();
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
    metrics.proxyError();
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
