const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');

// Load browser module in a synthetic global scope.
const fs = require('node:fs');
const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'cp437.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src + '\nmodule.exports = { cp437ToUtf8 };')(sandbox, sandbox);
const { cp437ToUtf8 } = sandbox.exports;

test('ASCII printable passes through unchanged', () => {
  const bytes = new Uint8Array([0x48, 0x69, 0x21]); // "Hi!"
  assert.strictEqual(cp437ToUtf8(bytes), 'Hi!');
});

test('control passthrough: CR/LF/TAB/ESC/BEL/BS/NUL preserved', () => {
  const bytes = new Uint8Array([0x00, 0x07, 0x08, 0x09, 0x0A, 0x0D, 0x1B]);
  assert.strictEqual(cp437ToUtf8(bytes), '\x00\x07\x08\x09\x0A\x0D\x1B');
});

test('CP437 low graphics: 0x01 -> ☺', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0x01])), '☺');
});

test('CP437 house at 0x7F -> ⌂', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0x7F])), '⌂');
});

test('CP437 high graphics: 0xB0 -> ░, 0xDB -> █', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0xB0])), '░');
  assert.strictEqual(cp437ToUtf8(new Uint8Array([0xDB])), '█');
});

test('empty buffer returns empty string', () => {
  assert.strictEqual(cp437ToUtf8(new Uint8Array([])), '');
});

test('full 0x00-0xFF round-trip matches expected length', () => {
  const bytes = new Uint8Array(256);
  for (let i = 0; i < 256; i++) bytes[i] = i;
  const out = cp437ToUtf8(bytes);
  // Every byte produces exactly one code point.
  assert.strictEqual([...out].length, 256);
});
