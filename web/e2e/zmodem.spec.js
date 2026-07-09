// E2E test: fake BBS runs `sz` against a fixture; browser (driven by
// playwright-cli) receives it via the notification bubble.
//
// Run: node web/e2e/zmodem.spec.js
//
// Requires: lrzsz (sz, rz), playwright-cli.

const net = require('node:net');
const { spawn } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const assert = require('node:assert');

const PW_SESSION = process.env.PILOT_SESSION_ID || 'zmodem-e2e';
const FIXTURE = path.join(__dirname, 'fixtures', 'hello.txt');

function runPW(args) {
  return new Promise((resolve, reject) => {
    const p = spawn('playwright-cli', ['-s=' + PW_SESSION, ...args], {
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    let out = '';
    p.stdout.on('data', d => out += d);
    p.on('exit', code => code === 0 ? resolve(out) : reject(new Error(`playwright-cli ${args[0]} exit ${code}`)));
  });
}

async function main() {
  // 1. Start a fake BBS listener that runs `sz` on connect.
  const bbs = net.createServer(conn => {
    const sz = spawn('sz', ['--zmodem', FIXTURE], { stdio: ['pipe', 'pipe', 'inherit'] });
    conn.pipe(sz.stdin);
    sz.stdout.pipe(conn);
    sz.on('exit', () => conn.end());
    conn.on('close', () => sz.kill());
  });
  await new Promise(r => bbs.listen(0, '127.0.0.1', r));
  const bbsPort = bbs.address().port;

  // 2. Start the Node proxy pointed at the fake BBS.
  const proxyEnv = { ...process.env, TELIX_HOST: '127.0.0.1', TELIX_PORT: String(bbsPort), PORT: '3123' };
  const proxy = spawn('node', [path.join(__dirname, '..', 'server.js')], {
    env: proxyEnv,
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  // Wait for proxy to log "listening" so we know the port is open.
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('proxy failed to start within 5s')), 5000);
    proxy.stdout.on('data', d => {
      if (String(d).includes('listening')) {
        clearTimeout(timer);
        resolve();
      }
    });
  });

  try {
    // 3. Drive the browser: open, wait for notification, close.
    await runPW(['open', 'http://127.0.0.1:3123']);
    // playwright-cli snapshot writes the accessibility tree to a YAML file and
    // prints a reference like `[Snapshot](.playwright-cli/page-...yml)` to stdout.
    // We need to read that YAML to see the actual DOM tree.
    let snapBody = '';
    let snapPath = '';
    for (let i = 0; i < 30; i++) {
      const out = await runPW(['snapshot']);
      const m = out.match(/\[Snapshot\]\(([^)]+)\)/);
      if (m) {
        snapPath = path.resolve(path.join(__dirname, '..'), m[1]);
        try {
          snapBody = fs.readFileSync(snapPath, 'utf8');
        } catch (_) {
          snapBody = '';
        }
      } else {
        // Fallback: some playwright-cli versions embed the tree in stdout.
        snapBody = out;
      }
      if (/hello\.txt/.test(snapBody)) break;
      await new Promise(r => setTimeout(r, 500));
    }
    assert.match(snapBody, /hello\.txt/, 'expected notification for hello.txt after sz ran');
    console.log('E2E download: notification surfaced OK');
  } finally {
    await runPW(['close']).catch(() => {});
    proxy.kill();
    bbs.close();
  }
}

main().catch(e => { console.error(e); process.exit(1); });
