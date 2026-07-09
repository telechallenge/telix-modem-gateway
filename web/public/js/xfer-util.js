// Pure utility functions for ZMODEM transfer UI. Kept separate from
// zmodem-ui.js so they can be unit-tested without a DOM.

function sanitizeFilename(name) {
  if (typeof name !== 'string' || name.length === 0) return 'download.bin';
  // Strip path components — take basename after any / or \.
  const parts = name.split(/[\\/]/);
  let base = parts[parts.length - 1] || '';
  // Restrict to safe chars.
  base = base.replace(/[^A-Za-z0-9._-]/g, '_');
  if (base.length === 0) return 'download.bin';
  if (base.length > 128) base = base.slice(0, 128);
  return base;
}

function formatBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  return (n / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function formatDuration(seconds) {
  if (!isFinite(seconds) || isNaN(seconds) || seconds < 0) return '--:--';
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return String(m).padStart(2, '0') + ':' + String(s).padStart(2, '0');
}

if (typeof window !== 'undefined') {
  window.XferUtil = { sanitizeFilename, formatBytes, formatDuration };
}
