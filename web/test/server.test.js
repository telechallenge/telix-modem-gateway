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
