const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'modem-state.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src + '\nmodule.exports = { parseResultCodes };')(sandbox, sandbox);
const { parseResultCodes } = sandbox.exports;

// Shape taken from a real ATDT to a BBS that banners immediately: the gateway
// writes CONNECT and then pumps the already-buffered banner, so both land in a
// single chunk (5673 bytes when captured).
const BANNER = '\x1b[2J\x1b[H\x1b[0;36m' + 'Fungus LAND BBS '.repeat(300);
const CONNECT_PLUS_BANNER = '\r\nCONNECT 33600/ARQ/V42/LAPM/V42BIS\r\n\x1b[0c\r\n' + BANNER;

test('finds CONNECT when it arrives coalesced with the BBS banner', () => {
  assert.ok(CONNECT_PLUS_BANNER.length > 200, 'fixture must exceed the old size gate');
  assert.deepStrictEqual(parseResultCodes(CONNECT_PLUS_BANNER), ['CONNECT']);
});

test('finds NO CARRIER when it arrives coalesced with a goodbye screen', () => {
  const goodbye = 'Thanks for calling!\r\n'.repeat(40) + '\r\nNO CARRIER\r\n';
  assert.deepStrictEqual(parseResultCodes(goodbye), ['NO CARRIER']);
});

test('finds a bare RING', () => {
  assert.deepStrictEqual(parseResultCodes('\r\nRING\r\n'), ['RING']);
});

test('returns codes in arrival order when several share a chunk', () => {
  assert.deepStrictEqual(
    parseResultCodes('\r\nRING\r\n\r\nCONNECT 56000\r\n'),
    ['RING', 'CONNECT']
  );
});

test('ignores result-code words embedded in BBS output', () => {
  const prose = [
    'ENTER A STRING TO SEARCH:\r\n',
    'BRINGING UP THE MESSAGE BASE\r\n',
    'DURING THE 1990S THIS BBS WAS BUSY EVERY NIGHT\r\n',
    'Sysop note: NO CARRIER means you were dropped.\r\n',
    'RINGS OF SATURN.ZIP\r\n',
  ].join('');
  assert.deepStrictEqual(parseResultCodes(prose), []);
});

test('requires a line boundary, not just a leading newline', () => {
  assert.deepStrictEqual(parseResultCodes('\r\nCONNECTED TO NODE 3\r\n'), []);
});
