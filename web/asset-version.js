// Cache-busting helpers. Kept out of server.js so they can be unit-tested
// without pulling in express/ws.
//
// Static JS/CSS is referenced from index.html with no version marker, so a
// browser — or the Cloudflare edge in front of the origin — keeps serving a
// cached copy after a redeploy, silently stranding users on old code. We stamp
// a content hash onto each local asset URL and serve index.html as no-cache, so
// a changed bundle always yields a fresh URL that no cache can satisfy stale.

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

// Short hash over every local .js/.css under `dir`. New content → new hash.
// Deterministic: entries are walked in sorted order.
function computeAssetVersion(dir) {
  const hash = crypto.createHash('sha1');
  const walk = d => {
    const entries = fs.readdirSync(d, { withFileTypes: true })
      .sort((a, b) => a.name.localeCompare(b.name));
    for (const entry of entries) {
      const full = path.join(d, entry.name);
      if (entry.isDirectory()) walk(full);
      else if (/\.(js|css)$/.test(entry.name)) {
        hash.update(entry.name);
        hash.update(fs.readFileSync(full));
      }
    }
  };
  walk(dir);
  return hash.digest('hex').slice(0, 12);
}

// Append ?v=<version> to local script/link URLs in `html`. Absolute URLs
// (the https:// CDN scripts) are left alone — only paths without a scheme and
// without an existing query are stamped.
function stampAssetUrls(html, version) {
  return html.replace(
    /(<(?:script|link)\b[^>]*\b(?:src|href)=")(js\/[^"?]+|[^":]+\.css)(")/g,
    (_, pre, url, post) => `${pre}${url}?v=${version}${post}`
  );
}

module.exports = { computeAssetVersion, stampAssetUrls };
