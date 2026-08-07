const test = require('node:test');
const assert = require('node:assert');
const { Metrics } = require('../metrics');

// The exposition text is the contract Prometheus reads, so assert on rendered
// output rather than on internal counter state.
function lines(m) {
  return m.render().split('\n');
}

function hasLine(m, want) {
  return lines(m).includes(want);
}

test('render emits HELP and TYPE metadata for each series', () => {
  const m = new Metrics();
  const out = m.render();

  assert.ok(out.includes('# HELP telixweb_ws_connections_active'), 'missing HELP line');
  assert.ok(out.includes('# TYPE telixweb_ws_connections_active gauge'), 'missing TYPE line');
  assert.ok(out.includes('# TYPE telixweb_ws_connections_total counter'), 'missing counter TYPE');
});

test('a connection raises the active gauge and the total counter', () => {
  const m = new Metrics();
  m.wsConnected();
  m.wsConnected();

  assert.ok(hasLine(m, 'telixweb_ws_connections_active 2'));
  assert.ok(hasLine(m, 'telixweb_ws_connections_total 2'));
});

test('a disconnect lowers the active gauge but never the total', () => {
  const m = new Metrics();
  m.wsConnected();
  m.wsConnected();
  m.wsDisconnected();

  assert.ok(hasLine(m, 'telixweb_ws_connections_active 1'));
  assert.ok(hasLine(m, 'telixweb_ws_connections_total 2'));
});

// cleanup() in server.js is deliberately idempotent, and both the ws 'close'
// and tcp 'close' handlers call it. If a stray extra disconnect could drive the
// gauge negative, the dashboard would show a nonsensical -1 connections.
test('the active gauge never goes negative', () => {
  const m = new Metrics();
  m.wsDisconnected();
  m.wsDisconnected();

  assert.ok(hasLine(m, 'telixweb_ws_connections_active 0'));
});

test('rejections and proxy errors are counted separately', () => {
  const m = new Metrics();
  m.wsRejected();
  m.wsRejected();
  m.proxyError();

  assert.ok(hasLine(m, 'telixweb_ws_rejected_total 2'));
  assert.ok(hasLine(m, 'telixweb_proxy_errors_total 1'));
});

test('bytes are counted per direction with labels', () => {
  const m = new Metrics();
  m.bytesToBrowser(100);
  m.bytesToBrowser(40);
  m.bytesToGateway(7);

  assert.ok(hasLine(m, 'telixweb_bytes_total{direction="to_browser"} 140'));
  assert.ok(hasLine(m, 'telixweb_bytes_total{direction="to_gateway"} 7'));
});

// Absent series make rate() return nothing, so a panel reads "No data" instead
// of a flat zero line. Every series must exist from the first scrape.
test('all series are present before any activity', () => {
  const m = new Metrics();
  const out = m.render();

  for (const name of [
    'telixweb_ws_connections_active 0',
    'telixweb_ws_connections_total 0',
    'telixweb_ws_rejected_total 0',
    'telixweb_proxy_errors_total 0',
    'telixweb_bytes_total{direction="to_browser"} 0',
    'telixweb_bytes_total{direction="to_gateway"} 0',
  ]) {
    assert.ok(out.includes(name), `missing series before activity: ${name}\n${out}`);
  }
});

test('process uptime and resident memory are exposed', () => {
  const m = new Metrics();
  const out = m.render();

  assert.match(out, /telixweb_process_uptime_seconds \d+(\.\d+)?/);
  assert.match(out, /telixweb_process_resident_memory_bytes \d+/);
});

// Prometheus rejects a body that does not end in a newline.
test('output ends with exactly one trailing newline', () => {
  const m = new Metrics();
  const out = m.render();

  assert.ok(out.endsWith('\n'), 'body must end with a newline');
  assert.ok(!out.endsWith('\n\n'), 'body must not end with a blank line');
});

test('counters render as integers, not exponential notation', () => {
  const m = new Metrics();
  m.bytesToBrowser(1e21);

  const line = lines(m).find(l => l.startsWith('telixweb_bytes_total{direction="to_browser"}'));
  assert.ok(!/e\+/i.test(line), `exponential notation is not valid exposition: ${line}`);
});
