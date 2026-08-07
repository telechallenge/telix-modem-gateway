// The Post-it stuck to the monitor bezel: a scratchpad for BBS numbers, handles
// and door codes. Notes never leave the browser — nothing here touches the
// WebSocket — and they are kept in localStorage, so they survive a reload and a
// return visit and go away only when the user clears the site's data.
//
// The store is split from the DOM wiring the way modem-state.js and
// xfer-util.js are, so the persistence rules can be unit-tested without a
// browser.

const NOTEPAD_KEY = 'telix.notepad.v1';
// A scratchpad, not a document store: one runaway paste should not be able to
// spend the origin's whole storage quota.
const NOTEPAD_MAX_CHARS = 8000;
const NOTEPAD_SAVE_DELAY_MS = 300;

function createNotepadStore(options) {
  const opts = options || {};
  const key = opts.key || NOTEPAD_KEY;
  const max = opts.max || NOTEPAD_MAX_CHARS;
  const getStorage = opts.getStorage || (() => window.localStorage);

  // Reaching localStorage throws outright — not merely failing to write — when
  // site data is blocked or the page is in a sandboxed frame, so even the
  // lookup is guarded. Without storage the pad still works for the session.
  let storage = null;
  try {
    storage = getStorage();
  } catch (e) {
    storage = null;
  }

  let text = '';
  let open = false;
  let writable = Boolean(storage);

  function read() {
    if (!storage) return null;
    try {
      return storage.getItem(key);
    } catch (e) {
      writable = false;
      return null;
    }
  }

  // Adopt a serialized value, field by field: anything missing or of the wrong
  // type falls back to the default rather than poisoning the pad. Returns
  // whether the state actually moved, so a no-op cross-tab event cannot make
  // the UI reset the caret.
  function apply(raw) {
    let nextText = '';
    let nextOpen = false;
    if (typeof raw === 'string') {
      try {
        const parsed = JSON.parse(raw);
        if (parsed && typeof parsed === 'object') {
          if (typeof parsed.text === 'string') nextText = parsed.text.slice(0, max);
          if (typeof parsed.open === 'boolean') nextOpen = parsed.open;
        }
      } catch (e) {
        // Corrupt value — open an empty pad instead of failing to load.
      }
    }
    const changed = nextText !== text || nextOpen !== open;
    text = nextText;
    open = nextOpen;
    return changed;
  }

  function write() {
    if (!storage) return;
    try {
      storage.setItem(key, JSON.stringify({ text, open }));
      writable = true;
    } catch (e) {
      // Over quota, or storage switched off mid-session. The note stays usable;
      // the UI says plainly that it is no longer being kept.
      writable = false;
    }
  }

  apply(read());

  return {
    text: () => text,
    isOpen: () => open,
    writable: () => writable,
    setText(value) {
      text = typeof value === 'string' ? value.slice(0, max) : '';
      write();
      return text;
    },
    setOpen(value) {
      open = Boolean(value);
      write();
      return open;
    },
    adopt: raw => apply(raw),
  };
}

function mountStickyNote(options) {
  const opts = options || {};
  const root = document.getElementById('stickyNote');
  const tab = document.getElementById('stickyTab');
  const body = document.getElementById('stickyBody');
  const area = document.getElementById('stickyText');
  const status = document.getElementById('stickyStatus');
  if (!root || !tab || !body || !area) return null;

  const store = opts.store || createNotepadStore({});
  let saveTimer = 0;

  area.maxLength = NOTEPAD_MAX_CHARS;

  function render() {
    const open = store.isOpen();
    root.dataset.open = open ? 'true' : 'false';
    root.dataset.hasText = area.value.trim().length > 0 ? 'true' : 'false';
    tab.setAttribute('aria-expanded', open ? 'true' : 'false');
    tab.title = open ? 'Hide notes' : 'Show notes';
    body.hidden = !open;
    if (status) {
      status.textContent = store.writable() ? '' : 'Not saved — browser storage is blocked';
    }
  }

  function save() {
    clearTimeout(saveTimer);
    saveTimer = 0;
    store.setText(area.value);
    render();
  }

  function scheduleSave() {
    root.dataset.hasText = area.value.trim().length > 0 ? 'true' : 'false';
    clearTimeout(saveTimer);
    saveTimer = setTimeout(save, NOTEPAD_SAVE_DELAY_MS);
  }

  function setOpen(open) {
    store.setOpen(open);
    render();
    if (open) area.focus();
    else if (typeof opts.onCollapse === 'function') opts.onCollapse();
  }

  tab.addEventListener('click', () => setOpen(!store.isOpen()));
  area.addEventListener('input', scheduleSave);

  // Escape hands the keyboard back to the terminal — the pad sits over the
  // screen, so leaving it is a thing the user needs to be able to do without
  // reaching for the mouse.
  area.addEventListener('keydown', e => {
    if (e.key !== 'Escape') return;
    e.stopPropagation();
    save();
    setOpen(false);
  });

  // A debounced save loses the last keystrokes when the tab goes away, and
  // `beforeunload` does not fire reliably on mobile. These two do.
  window.addEventListener('pagehide', () => { if (saveTimer) save(); });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden' && saveTimer) save();
  });

  // Another tab editing the same pad. Skipped while this one has the caret in
  // the note: last write wins there, which beats yanking text out from under
  // someone mid-sentence.
  window.addEventListener('storage', e => {
    if (e.key !== NOTEPAD_KEY || document.activeElement === area) return;
    if (!store.adopt(e.newValue)) return;
    area.value = store.text();
    render();
  });

  area.value = store.text();
  render();

  return { store, save };
}

if (typeof window !== 'undefined') {
  window.StickyNote = { createNotepadStore, mountStickyNote };
}
