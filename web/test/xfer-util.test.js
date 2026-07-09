const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'xfer-util.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src + '\nmodule.exports = { sanitizeFilename, formatBytes, formatDuration };')(sandbox, sandbox);
const { sanitizeFilename, formatBytes, formatDuration } = sandbox.exports;

test('sanitizeFilename strips path components', () => {
  assert.strictEqual(sanitizeFilename('../../etc/passwd'), 'passwd');
  assert.strictEqual(sanitizeFilename('C:\\Users\\me\\file.zip'), 'file.zip');
});

test('sanitizeFilename replaces unsafe chars with underscore', () => {
  assert.strictEqual(sanitizeFilename('bad name?.txt'), 'bad_name_.txt');
  assert.strictEqual(sanitizeFilename('中文.zip'), '__.zip');
});

test('sanitizeFilename allows [A-Za-z0-9._-]', () => {
  assert.strictEqual(sanitizeFilename('OK_file-1.2.zip'), 'OK_file-1.2.zip');
});

test('sanitizeFilename caps length at 128', () => {
  const long = 'a'.repeat(200) + '.zip';
  const out = sanitizeFilename(long);
  assert.strictEqual(out.length, 128);
});

test('sanitizeFilename empty or all-invalid -> download.bin', () => {
  assert.strictEqual(sanitizeFilename(''), 'download.bin');
  assert.strictEqual(sanitizeFilename('///'), 'download.bin');
  assert.strictEqual(sanitizeFilename(null), 'download.bin');
});

test('formatBytes: human-readable', () => {
  assert.strictEqual(formatBytes(0), '0 B');
  assert.strictEqual(formatBytes(512), '512 B');
  assert.strictEqual(formatBytes(1024), '1.0 KB');
  assert.strictEqual(formatBytes(1536), '1.5 KB');
  assert.strictEqual(formatBytes(1048576), '1.0 MB');
  assert.strictEqual(formatBytes(1073741824), '1.0 GB');
});

test('formatDuration: MM:SS', () => {
  assert.strictEqual(formatDuration(0), '00:00');
  assert.strictEqual(formatDuration(65), '01:05');
  assert.strictEqual(formatDuration(3599), '59:59');
  assert.strictEqual(formatDuration(Infinity), '--:--');
  assert.strictEqual(formatDuration(NaN), '--:--');
});
