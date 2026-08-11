const test = require('node:test');
const assert = require('node:assert');
const net = require('node:net');
const { WebSocket } = require('ws');

// Spawn a fake Telix TCP server so we can drive the Node proxy end-to-end.
async function withProxy(callback) {
  const backend = net.createServer();
  const backendConns = [];
  backend.on('connection', c => backendConns.push(c));
  await new Promise(r => backend.listen(0, '127.0.0.1', r));
  const backendPort = backend.address().port;

  process.env.TELIX_HOST = '127.0.0.1';
  process.env.TELIX_PORT = String(backendPort);
  process.env.PORT = '0';
  delete require.cache[require.resolve('../server.js')];
  const serverModule = require('../server.js');

  // Wait for the listener to be bound. server.listen() returns immediately;
  // address() returns null until the underlying socket is bound.
  await new Promise((resolve, reject) => {
    if (serverModule.listening) return resolve();
    serverModule.once('listening', resolve);
    serverModule.once('error', reject);
  });
  const wsPort = serverModule.address().port;

  try {
    await callback({ wsPort, backendConns });
  } finally {
    await new Promise(r => serverModule.close(r));
    await new Promise(r => backend.close(r));
    for (const c of backendConns) c.destroy();
  }
}

test('WebSocket receives backend bytes as binary frames (not text)', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise((resolve, reject) => {
      ws.on('open', resolve);
      ws.on('error', reject);
    });

    // Wait for backend to accept.
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      ws.on('message', (data, isBinary) => resolve({ data, isBinary }));
    });

    backend.write(Buffer.from([0xFF, 0x00, 0x1B, 0x5B, 0x41])); // includes 0xFF and ESC

    const { data, isBinary } = await received;
    assert.strictEqual(isBinary, true, 'expected binary frame');
    assert.deepStrictEqual(new Uint8Array(data), new Uint8Array([0xFF, 0x00, 0x1B, 0x5B, 0x41]));
    ws.close();
  });
});

test('outbound 0xFF from browser is escaped to 0xFF 0xFF before reaching backend', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise(r => ws.on('open', r));
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      const chunks = [];
      backend.on('data', d => {
        chunks.push(d);
        const total = Buffer.concat(chunks);
        if (total.length >= 4) resolve(total);
      });
    });

    // Send bytes containing an IAC (0xFF) as raw binary WS payload.
    ws.send(Buffer.from([0x41, 0xFF, 0x42])); // "A", IAC, "B"

    const got = await received;
    assert.deepStrictEqual(
      [...got.subarray(0, 4)],
      [0x41, 0xFF, 0xFF, 0x42],
      'IAC should be doubled'
    );
    ws.close();
  });
});

test('outbound bytes without 0xFF pass through unchanged', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise(r => ws.on('open', r));
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const received = new Promise(resolve => {
      backend.on('data', d => resolve(d));
    });

    ws.send(Buffer.from('hello'));
    const got = await received;
    assert.deepStrictEqual([...got], [...Buffer.from('hello')]);
    ws.close();
  });
});

// One request, headers and body both returned, so the header tests can assert
// against the page that actually carries the CDN <script> tags.
function request(port, path, headers = {}) {
  return new Promise((resolve, reject) => {
    require('node:http').get({ host: '127.0.0.1', port, path, headers }, res => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: data }));
    }).on('error', reject);
  });
}

test('the page is served with the security headers it needs', async () => {
  await withProxy(async ({ wsPort }) => {
    const res = await request(wsPort, '/');
    const csp = res.headers['content-security-policy'];

    assert.ok(csp, 'no CSP header');
    // Anything not named falls back to same-origin.
    assert.match(csp, /(^|;\s*)default-src 'self'(;|$)/);
    // The one third-party origin the page legitimately loads code from.
    assert.match(csp, /script-src [^;]*'self'[^;]*https:\/\/cdn\.jsdelivr\.net/);
    // xterm builds its own <style> element at runtime, so inline style has to
    // be allowed — inline *script* must not be.
    assert.match(csp, /style-src [^;]*'unsafe-inline'/);
    assert.doesNotMatch(csp, /script-src [^;]*'unsafe-inline'/);
    assert.doesNotMatch(csp, /script-src [^;]*'unsafe-eval'/);
    // The WebSocket is same-origin; no other host may be dialled.
    assert.match(csp, /connect-src 'self'(;|$)/);
    // Clickjacking, base-tag hijacking, plugin content, form exfiltration.
    assert.match(csp, /frame-ancestors 'none'/);
    assert.match(csp, /base-uri 'none'/);
    assert.match(csp, /object-src 'none'/);
    assert.match(csp, /form-action 'none'/);

    assert.strictEqual(res.headers['x-frame-options'], 'DENY');
    assert.strictEqual(res.headers['x-content-type-options'], 'nosniff');
    assert.strictEqual(res.headers['referrer-policy'], 'no-referrer');
    assert.ok(res.headers['permissions-policy'], 'no Permissions-Policy header');
    assert.strictEqual(res.headers['x-powered-by'], undefined, 'Express banner still advertised');
  });
});

test('static assets and the metrics endpoint carry the headers too', async () => {
  await withProxy(async ({ wsPort }) => {
    for (const path of ['/js/app.js', '/metrics', '/config.json']) {
      const res = await request(wsPort, path);
      assert.ok(res.headers['content-security-policy'], `no CSP on ${path}`);
      assert.strictEqual(res.headers['x-content-type-options'], 'nosniff', `no nosniff on ${path}`);
    }
  });
});

test('HSTS is sent only once the request actually arrived over TLS', async () => {
  await withProxy(async ({ wsPort }) => {
    // Plain HTTP: a browser would ignore the header anyway, and sending it
    // would promise TLS for a deployment that may not have any.
    const plain = await request(wsPort, '/');
    assert.strictEqual(plain.headers['strict-transport-security'], undefined);

    // Behind the nginx/Cloudflare front, which sets X-Forwarded-Proto.
    const tls = await request(wsPort, '/', { 'X-Forwarded-Proto': 'https' });
    assert.match(tls.headers['strict-transport-security'] || '', /^max-age=\d+/);
  });
});

test('every CDN asset the page loads is pinned with an integrity hash', async () => {
  await withProxy(async ({ wsPort }) => {
    const html = (await request(wsPort, '/')).body;
    const tags = html.match(/<(?:script|link)\b[^>]*(?:src|href)="https:\/\/[^"]+"[^>]*>/g) || [];

    assert.ok(tags.length >= 3, `expected the CDN tags, found ${tags.length}`);
    for (const tag of tags) {
      assert.match(tag, /integrity="sha(256|384|512)-[A-Za-z0-9+/=]+"/, `unpinned CDN asset: ${tag}`);
      // Without CORS the browser cannot check the hash and blocks the load.
      assert.match(tag, /crossorigin="anonymous"/, `integrity without crossorigin: ${tag}`);
    }
  });
});

test('GET /config.json returns MAX_UPLOAD_BYTES and ZMODEM_TIMEOUT_SEC', async () => {
  process.env.MAX_UPLOAD_BYTES = '2048';
  process.env.ZMODEM_TIMEOUT_SEC = '15';
  await withProxy(async ({ wsPort }) => {
    const body = await new Promise((resolve, reject) => {
      require('node:http').get(`http://127.0.0.1:${wsPort}/config.json`, res => {
        let data = '';
        res.on('data', c => data += c);
        res.on('end', () => resolve({ status: res.statusCode, body: JSON.parse(data) }));
      }).on('error', reject);
    });
    assert.strictEqual(body.status, 200);
    assert.strictEqual(body.body.maxUploadBytes, 2048);
    assert.strictEqual(body.body.zmodemTimeoutSec, 15);
  });
  delete process.env.MAX_UPLOAD_BYTES;
  delete process.env.ZMODEM_TIMEOUT_SEC;
});

test('GET /config.json defaults: 1 GiB / 30 s', async () => {
  delete process.env.MAX_UPLOAD_BYTES;
  delete process.env.ZMODEM_TIMEOUT_SEC;
  await withProxy(async ({ wsPort }) => {
    const body = await new Promise((resolve, reject) => {
      require('node:http').get(`http://127.0.0.1:${wsPort}/config.json`, res => {
        let data = '';
        res.on('data', c => data += c);
        res.on('end', () => resolve(JSON.parse(data)));
      }).on('error', reject);
    });
    assert.strictEqual(body.maxUploadBytes, 1073741824);
    assert.strictEqual(body.zmodemTimeoutSec, 30);
  });
});

test('GET /config.json exposes the ZMODEM block size, auto by default', async () => {
  delete process.env.ZMODEM_BLOCK_SIZE;
  await withProxy(async ({ wsPort }) => {
    assert.strictEqual((await getConfig(wsPort)).zmodemBlockSize, 0, 'default is auto');
  });

  process.env.ZMODEM_BLOCK_SIZE = '8192';
  await withProxy(async ({ wsPort }) => {
    assert.strictEqual((await getConfig(wsPort)).zmodemBlockSize, 8192);
  });

  // Only 1024 and 8192 mean anything on the wire; anything else must fall back
  // to auto rather than putting an arbitrary subpacket size in front of a BBS.
  process.env.ZMODEM_BLOCK_SIZE = '4096';
  await withProxy(async ({ wsPort }) => {
    assert.strictEqual((await getConfig(wsPort)).zmodemBlockSize, 0);
  });
  delete process.env.ZMODEM_BLOCK_SIZE;
});

function getConfig(wsPort) {
  return new Promise((resolve, reject) => {
    require('node:http').get(`http://127.0.0.1:${wsPort}/config.json`, res => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => resolve(JSON.parse(data)));
    }).on('error', reject);
  });
}

// net.Socket#write buffers in the heap and returns false past its high-water
// mark. Ignoring that meant a ZMODEM upload the gateway could not drain simply
// relocated into this process — the browser watched its own socket empty,
// concluded the file had landed and started its idle timer while the bytes were
// still queued here. The fix pauses the WebSocket until the gateway catches up,
// so what this has to prove is the other half: that it resumes, and that every
// byte still arrives in order.
test('a slow gateway backpressures the browser without losing or reordering bytes', async () => {
  await withProxy(async ({ wsPort, backendConns }) => {
    const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`);
    await new Promise((resolve, reject) => { ws.on('open', resolve); ws.on('error', reject); });
    while (backendConns.length === 0) await new Promise(r => setTimeout(r, 10));
    const backend = backendConns[0];

    const CHUNK = 64 * 1024;
    const CHUNKS = 64; // 4 MiB, comfortably past any socket buffer in the path
    let got = 0;
    let ordered = true;
    // Read in bursts with the socket paused in between: the gateway here is the
    // slow end, which is the case the pause/resume path exists for.
    backend.pause();
    const reader = setInterval(() => {
      const buf = backend.read();
      if (!buf) return;
      for (const b of buf) {
        if (b !== 1 + (got % 254)) ordered = false;
        got++;
      }
    }, 5);

    for (let i = 0; i < CHUNKS; i++) {
      const buf = Buffer.alloc(CHUNK);
      // A ramp over 0x01..0xFE. It must avoid 0xFF, which escapeIAC doubles so
      // the count below would stop being a straight one, and 0x00, which the
      // proxy reads as the leading byte of a NAWS resize frame — a payload
      // starting with it is swallowed and replaced by 9 bytes of NAWS.
      for (let j = 0; j < CHUNK; j++) buf[j] = 1 + ((i * CHUNK + j) % 254);
      ws.send(buf, { binary: true });
    }

    const deadline = Date.now() + 20000;
    while (got < CHUNK * CHUNKS && Date.now() < deadline) {
      await new Promise(r => setTimeout(r, 10));
    }
    clearInterval(reader);
    ws.close();

    assert.strictEqual(got, CHUNK * CHUNKS, `the gateway did not receive every byte (got ${got})`);
    assert.ok(ordered, 'bytes arrived out of order or corrupted');
  });
});

// nginx *appends* to whatever X-Forwarded-For the client sent
// ($proxy_add_x_forwarded_for = "$http_x_forwarded_for, $remote_addr"), so the
// first entry of that header is attacker-controlled. Reading it as the client
// identity hands out a fresh MAX_WS_PER_IP budget for every forged value.
// X-Real-IP is the one nginx *sets* — it overwrites any client-supplied copy —
// so it is the only forwarded header here that a caller cannot choose.
test('a forged X-Forwarded-For cannot buy extra connections past MAX_WS_PER_IP', async () => {
  const prevMax = process.env.MAX_WS_PER_IP;
  process.env.MAX_WS_PER_IP = '2';
  try {
    await withProxy(async ({ wsPort }) => {
      const sockets = [];
      const closes = [];

      // Three calls from one real client (203.0.113.7), each forging a
      // different first XFF entry — exactly what nginx forwards when the
      // browser sets its own X-Forwarded-For.
      for (let i = 0; i < 3; i++) {
        const ws = new WebSocket(`ws://127.0.0.1:${wsPort}/ws`, {
          headers: {
            'X-Real-IP': '203.0.113.7',
            'X-Forwarded-For': `198.51.100.${i + 1}, 203.0.113.7`,
          },
        });
        ws.on('close', code => closes.push(code));
        sockets.push(ws);
        await new Promise((resolve, reject) => {
          ws.once('open', resolve);
          ws.once('error', reject);
        });
      }

      // The rejection is a close frame sent straight after the handshake, so
      // it lands just behind 'open'. Give it a beat to arrive.
      const deadline = Date.now() + 2000;
      while (closes.length === 0 && Date.now() < deadline) {
        await new Promise(r => setTimeout(r, 10));
      }
      for (const ws of sockets) ws.close();

      assert.deepStrictEqual(
        closes, [1008],
        `expected the third call to be rejected as over the per-IP limit, got closes: ${JSON.stringify(closes)}`,
      );
    });
  } finally {
    if (prevMax === undefined) delete process.env.MAX_WS_PER_IP;
    else process.env.MAX_WS_PER_IP = prevMax;
  }
});
