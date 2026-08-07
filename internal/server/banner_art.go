package server

import (
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// Largest .ANS the gateway will draw. Scene art runs to a few tens of KB; a
// file past this is not something anyone wants blasted down a modem session,
// and the cap stops one stray file in a mounted directory from becoming a
// memory or bandwidth problem. Oversized files are skipped rather than
// truncated — half a file is a screen of garbage, and the built-in banner is a
// better answer than that.
const maxBannerArtBytes = 512 * 1024

// Art assumes it owns a cleared screen starting at the home position, and it
// routinely ends mid-colour — without the reset the OK that follows inherits
// whatever the last escape sequence set.
//
// The park is the load-bearing part. A full-screen piece fills the grid to the
// last cell, and the result code written straight after it is "\r\nOK\r\n" —
// two line feeds from the bottom row, so the terminal scrolls twice and the top
// two rows of the art are gone before the caller ever sees them. It is not
// recoverable by trimming the art either: these pieces pad their lower rows
// with 0xDB blocks drawn in black, which read as empty screen but are real
// cells. So the cursor is parked two rows short of the bottom and the result
// code is allowed to land there instead. ESC[999;1H clamps to the last row
// whatever the client's height is, and ESC[2A backs up from it, so this holds
// on an 80x24 web terminal and an 80x25 telnet client alike.
const (
	artPrefix = "\x1b[0m\x1b[2J\x1b[H"
	artSuffix = "\x1b[0m\x1b[999;1H\x1b[2A"
)

var errArtTooBig = errors.New("banner art exceeds the size cap")

// bannerArt draws the connect banner from a directory of ANSI art mounted into
// the container, choosing a different file per session.
//
// Nothing is cached. The directory is expected to change under a running
// gateway — that is the whole point of mounting it — and the obvious cache,
// keyed on the directory's mtime, is quietly broken: Linux serves a coarse
// cached clock to filesystem timestamps, so a file dropped in within the same
// tick as the last scan leaves the mtime byte-identical and the new art would
// never be drawn. A TTL only narrows that window. Re-reading is also cheap
// where it happens: one readdir plus one small file read per *connection*, on a
// path that already pays for a TCP handshake and sleeps 100ms waiting out
// telnet negotiation, and accepts are rate-limited to 50/sec besides. Reading
// the chosen file per session also keeps a large art pack off the heap.
type bannerArt struct {
	dir string
}

func newBannerArt(dir string) *bannerArt {
	return &bannerArt{dir: strings.TrimSpace(dir)}
}

// pick returns one random piece of art, ready to write to the client, or an
// empty string when the directory is unset, unreadable, or holds nothing
// drawable — in which case the caller falls back to the built-in banner.
func (b *bannerArt) pick() string {
	names := b.list()
	if len(names) == 0 {
		return ""
	}

	raw, err := b.read(names[rand.Intn(len(names))])
	if err != nil {
		// Deleted between the listing and the read, unreadable, or oversized.
		// One connection gets the built-in banner; nothing else is affected.
		return ""
	}
	return renderArt(raw)
}

// list returns the drawable filenames currently in the directory.
func (b *bannerArt) list() []string {
	if b.dir == "" {
		return nil
	}
	info, err := os.Stat(b.dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".ans") {
			continue
		}
		names = append(names, name)
	}
	return names
}

func (b *bannerArt) read(name string) ([]byte, error) {
	f, err := os.Open(filepath.Join(b.dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// One byte past the cap, so an oversized file is detectable rather than
	// silently truncated to exactly the limit.
	raw, err := io.ReadAll(io.LimitReader(f, maxBannerArtBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBannerArtBytes {
		return nil, errArtTooBig
	}
	return raw, nil
}

// renderArt turns the bytes of a .ANS file into something safe to write to a
// terminal that is about to show an AT prompt.
func renderArt(raw []byte) string {
	art := normalizeNewlines(stripSAUCE(raw))
	return artPrefix + string(art) + artSuffix
}

// SAUCE record layout. The trailer is a fixed 128 bytes; the comment count at
// offset 104 says how many 64-byte lines the optional COMNT block before it
// holds.
const (
	sauceSize        = 128
	sauceID          = "SAUCE00"
	sauceCommentsOff = 104
	commentID        = "COMNT"
	commentLineSize  = 64
)

// stripSAUCE removes the metadata trailer the DOS art tools append. Left in
// place it prints as a line of mojibake under the art — the title, author and
// group of the piece.
//
// It is matched explicitly rather than by truncating at the DOS EOF byte that
// usually precedes it: 0x1A is a perfectly good CP437 glyph (a right arrow) and
// art does use it, so cutting at the first one would eat real artwork.
func stripSAUCE(raw []byte) []byte {
	if len(raw) < sauceSize {
		return raw
	}
	rec := raw[len(raw)-sauceSize:]
	if string(rec[:len(sauceID)]) != sauceID {
		return raw
	}

	cut := len(raw) - sauceSize

	if lines := int(rec[sauceCommentsOff]); lines > 0 {
		size := len(commentID) + lines*commentLineSize
		if cut >= size && string(raw[cut-size:cut-size+len(commentID)]) == commentID {
			cut -= size
		}
	}

	// The EOF marker, when the tool wrote one. Safe to trust here: we have
	// already proved a SAUCE record follows it.
	if cut > 0 && raw[cut-1] == 0x1A {
		cut--
	}
	return raw[:cut]
}

// normalizeNewlines gives every bare line feed its carriage return back.
// DOS-authored art already carries CRLF and passes through untouched; a file
// saved on a Unix box would otherwise staircase down the screen, since the
// client is in raw mode and nothing else puts the cursor back to column 0.
func normalizeNewlines(art []byte) []byte {
	out := make([]byte, 0, len(art))
	for i, c := range art {
		if c == '\n' && (i == 0 || art[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, c)
	}
	return out
}

// bannerFor is what a session greets a caller with: a random piece of art when
// a directory of it is mounted and readable, the built-in banner otherwise.
func bannerFor(art *bannerArt, version string) string {
	if art != nil {
		if drawn := art.pick(); drawn != "" {
			return drawn
		}
	}
	return buildBanner(version)
}
