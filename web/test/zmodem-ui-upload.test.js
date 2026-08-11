// Verifies the upload prompt's DOM wiring: the file picker must be opened from
// a real button click (transient user activation), not from the network event
// that requests the upload — browsers block input.click() without a gesture, so
// the old direct-click never opened a dialog. No real browser here, so this
// stubs just enough DOM to prove the wiring; it cannot prove the OS dialog
// actually renders (that needs a user gesture no automation can supply).

const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');
const vm = require('node:vm');

function makeEl(tag) {
  const listeners = {};
  const el = {
    tagName: tag, type: '', textContent: '', className: '', hidden: false,
    style: {}, files: [], value: '',
    children: [],
    dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    setAttribute(k, v) { el[k] = v; },
    addEventListener: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
    removeEventListener: (ev, fn) => { listeners[ev] = (listeners[ev] || []).filter(f => f !== fn); },
    appendChild: c => { el.children.push(c); return c; },
    removeChild: c => { el.children = el.children.filter(x => x !== c); },
    remove() { if (el._parent) el._parent.removeChild(el); },
    click() { el._clicks = (el._clicks || 0) + 1; (listeners['click'] || []).forEach(f => f()); },
    dispatch(ev) { (listeners[ev] || []).forEach(f => f()); },
    querySelectorAll() { return []; },
  };
  const origAppend = el.appendChild;
  el.appendChild = c => { c._parent = el; return origAppend(c); };
  return el;
}

function loadUI(opts) {
  const ids = ['xferStrip', 'xferDir', 'xferName', 'xferPct', 'xferFill', 'xferCps',
    'xferEta', 'xferBatch', 'xferNotifications', 'xferUploadInput', 'muteBtn', 'muteIcon'];
  const els = {};
  for (const id of ids) els[id] = makeEl('div');
  const uploadInput = els.xferUploadInput;

  const document = {
    getElementById: id => els[id] || null,
    createElement: tag => makeEl(tag),
    querySelectorAll: () => [],
    addEventListener() {},
    body: makeEl('body'),
  };
  const store = (opts && opts.store) || new Map();
  const g = {
    console, document,
    window: null,
    setTimeout, clearTimeout,
    URL: { createObjectURL: () => 'blob:x', revokeObjectURL() {} },
  };
  // Site data blocked (or a sandboxed frame) makes even the *property lookup*
  // throw, not just the write, so model it as a throwing getter rather than as
  // methods that fail.
  if (opts && opts.storageBlocked) {
    Object.defineProperty(g, 'localStorage', {
      get() { throw new Error('SecurityError: access denied'); },
    });
  } else {
    g.localStorage = {
      getItem: k => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
    };
  }
  g.window = g;
  g.window.XferUtil = { sanitizeFilename: s => s, formatBytes: () => '1 KB', formatDuration: () => '0s' };
  vm.createContext(g);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'zmodem-ui.js'), 'utf8'), g, { filename: 'zmodem-ui.js' });
  return { ui: g.window.ZmodemUI, uploadInput, notifications: els.xferNotifications, store };
}

test('promptUpload does NOT open the picker until the user clicks the button', () => {
  const { ui, uploadInput, notifications } = loadUI();
  ui.promptUpload(1 << 30);

  // The network event alone must not have triggered a picker.
  assert.strictEqual(uploadInput._clicks || 0, 0, 'picker opened without a user gesture');
  // A prompt bubble with a button should be present instead.
  assert.strictEqual(notifications.children.length, 1, 'expected an upload prompt bubble');
  const bubble = notifications.children[0];
  const chooseBtn = bubble.children.find(c => c.textContent === 'Choose files');
  assert.ok(chooseBtn, 'expected a "Choose files" button');

  // Clicking it (a real gesture) opens the picker.
  chooseBtn.click();
  assert.strictEqual(uploadInput._clicks, 1, 'button click should open the picker');
});

test('picking files resolves promptUpload with those files and clears the prompt', async () => {
  const { ui, uploadInput, notifications } = loadUI();
  const p = ui.promptUpload(1 << 30);

  const bubble = notifications.children[0];
  bubble.children.find(c => c.textContent === 'Choose files').click();

  uploadInput.files = [{ name: 'a.txt', size: 10 }, { name: 'b.txt', size: 20 }];
  uploadInput.dispatch('change');

  const files = await p;
  // `files` is created inside the vm sandbox, so its Array prototype differs
  // from this realm's — compare by value, not with deepStrictEqual on arrays.
  assert.strictEqual(Array.from(files, f => f.name).join(','), 'a.txt,b.txt');
  assert.strictEqual(notifications.children.length, 0, 'prompt should be dismissed after picking');
});

test('cancelling the prompt resolves with an empty list', async () => {
  const { ui, notifications } = loadUI();
  const p = ui.promptUpload(1 << 30);

  const bubble = notifications.children[0];
  bubble.children.find(c => c.textContent === 'Cancel').click();

  const files = await p;
  assert.strictEqual(Array.from(files).length, 0);
  assert.strictEqual(notifications.children.length, 0);
});

test('a native picker cancel (no files) leaves the prompt up for a retry', () => {
  const { ui, uploadInput, notifications } = loadUI();
  ui.promptUpload(1 << 30);

  const bubble = notifications.children[0];
  bubble.children.find(c => c.textContent === 'Choose files').click();

  // User opened the dialog then cancelled it → change fires with no files.
  uploadInput.files = [];
  uploadInput.dispatch('change');

  assert.strictEqual(notifications.children.length, 1, 'prompt should remain so the user can retry');
});
