const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');

const src = fs.readFileSync(path.join(__dirname, '..', 'public', 'js', 'sticky-note.js'), 'utf8');
const sandbox = {};
new Function('module', 'exports', src +
  '\nmodule.exports = { createNotepadStore, NOTEPAD_KEY, NOTEPAD_MAX_CHARS };')(sandbox, sandbox);
const { createNotepadStore, NOTEPAD_KEY, NOTEPAD_MAX_CHARS } = sandbox.exports;

// A stand-in for localStorage. `onSet` lets a test make writes fail the way a
// browser does when the origin is over quota.
function fakeStorage(onSet) {
  const map = new Map();
  return {
    getItem: k => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => {
      if (onSet) onSet(k, v);
      map.set(k, String(v));
    },
    removeItem: k => { map.delete(k); },
    raw: () => map.get(NOTEPAD_KEY),
  };
}

const storeOver = storage => createNotepadStore({ getStorage: () => storage });

test('notes written in one visit are still there on the next', () => {
  const storage = fakeStorage();
  storeOver(storage).setText('Fungus LAND — 555-0134, handle: sixpack');

  // A later visit is a fresh store over the same browser storage.
  assert.strictEqual(storeOver(storage).text(), 'Fungus LAND — 555-0134, handle: sixpack');
});

test('the pad reopens in the state it was left in', () => {
  const storage = fakeStorage();
  const first = storeOver(storage);
  assert.strictEqual(first.isOpen(), false, 'starts closed on a first visit');
  first.setOpen(true);

  assert.strictEqual(storeOver(storage).isOpen(), true);
});

test('caps what it will hold so one paste cannot fill the origin quota', () => {
  const storage = fakeStorage();
  const store = storeOver(storage);

  const kept = store.setText('x'.repeat(NOTEPAD_MAX_CHARS + 500));

  assert.strictEqual(kept.length, NOTEPAD_MAX_CHARS);
  assert.strictEqual(store.text().length, NOTEPAD_MAX_CHARS);
  assert.strictEqual(storeOver(storage).text().length, NOTEPAD_MAX_CHARS);
});

test('a corrupt stored value opens an empty pad instead of throwing', () => {
  const storage = fakeStorage();
  storage.setItem(NOTEPAD_KEY, '{not json at all');

  const store = storeOver(storage);

  assert.strictEqual(store.text(), '');
  assert.strictEqual(store.isOpen(), false);
  assert.strictEqual(store.setText('recovered'), 'recovered');
});

test('a stored value of the wrong shape is ignored field by field', () => {
  const storage = fakeStorage();
  storage.setItem(NOTEPAD_KEY, JSON.stringify({ text: { nope: 1 }, open: 'yes' }));

  const store = storeOver(storage);

  assert.strictEqual(store.text(), '');
  assert.strictEqual(store.isOpen(), false);
});

test('still takes notes when storage is blocked, and says it is not saving', () => {
  // Reading `localStorage` throws outright when site data is blocked or the
  // page is in a sandboxed frame — not just writing to it.
  const store = createNotepadStore({
    getStorage: () => { throw new Error('SecurityError'); },
  });

  assert.strictEqual(store.writable(), false);
  assert.strictEqual(store.setText('scratch'), 'scratch');
  assert.strictEqual(store.text(), 'scratch');
});

test('keeps the text in the session when the write is refused', () => {
  const storage = fakeStorage(() => { throw new Error('QuotaExceededError'); });
  const store = createNotepadStore({ getStorage: () => storage });

  assert.strictEqual(store.setText('typed anyway'), 'typed anyway');
  assert.strictEqual(store.text(), 'typed anyway');
  assert.strictEqual(store.writable(), false);
});

test('picks up an edit made in another tab', () => {
  const storage = fakeStorage();
  const store = storeOver(storage);
  store.setText('here');

  assert.strictEqual(store.adopt(JSON.stringify({ text: 'there', open: true })), true);
  assert.strictEqual(store.text(), 'there');
  assert.strictEqual(store.isOpen(), true);

  // Same value again is not a change — the UI must not clobber the caret for it.
  assert.strictEqual(store.adopt(JSON.stringify({ text: 'there', open: true })), false);
});

test('another tab clearing site data empties the pad', () => {
  const storage = fakeStorage();
  const store = storeOver(storage);
  store.setText('here');

  assert.strictEqual(store.adopt(null), true);
  assert.strictEqual(store.text(), '');
});
