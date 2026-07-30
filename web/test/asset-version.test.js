const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');

const { computeAssetVersion, stampAssetUrls } = require('../asset-version');

test('stampAssetUrls versions local scripts and styles but not CDN URLs', () => {
  const html = [
    '<link rel="stylesheet" href="https://cdn.example.com/x.css">',
    '<link rel="stylesheet" href="css/terminal.css">',
    '<script src="https://cdn.example.com/lib.js"></script>',
    '<script src="js/app.js"></script>',
    '<script src="js/vendor/zmodem.min.js"></script>',
  ].join('\n');

  const out = stampAssetUrls(html, 'abc123');

  assert.match(out, /href="css\/terminal\.css\?v=abc123"/);
  assert.match(out, /src="js\/app\.js\?v=abc123"/);
  assert.match(out, /src="js\/vendor\/zmodem\.min\.js\?v=abc123"/);
  // CDN URLs untouched — a strict CSP / SRI would break if we rewrote them.
  assert.match(out, /href="https:\/\/cdn\.example\.com\/x\.css"/);
  assert.match(out, /src="https:\/\/cdn\.example\.com\/lib\.js"/);
  assert.strictEqual(out.match(/cdn\.example\.com[^"]*\?v=/g), null);
});

test('stampAssetUrls does not double-stamp an already-queried URL', () => {
  const html = '<script src="js/app.js?v=old"></script>';
  // The regex only matches URLs without an existing "?".
  assert.strictEqual(stampAssetUrls(html, 'new'), html);
});

test('computeAssetVersion changes when a file changes and is stable otherwise', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'assetver-'));
  fs.mkdirSync(path.join(dir, 'js'));
  fs.writeFileSync(path.join(dir, 'js', 'a.js'), 'console.log(1)');
  fs.writeFileSync(path.join(dir, 'css'), ''); // a non-.js/.css file is ignored below

  const v1 = computeAssetVersion(dir);
  assert.match(v1, /^[a-f0-9]{12}$/);

  // Same content → same version.
  assert.strictEqual(computeAssetVersion(dir), v1);

  // Changed content → different version (this is what busts caches).
  fs.writeFileSync(path.join(dir, 'js', 'a.js'), 'console.log(2)');
  assert.notStrictEqual(computeAssetVersion(dir), v1);
});

test('computeAssetVersion produces the real bundle version deterministically', () => {
  const publicDir = path.join(__dirname, '..', 'public');
  const a = computeAssetVersion(publicDir);
  const b = computeAssetVersion(publicDir);
  assert.strictEqual(a, b);
  assert.match(a, /^[a-f0-9]{12}$/);
});
