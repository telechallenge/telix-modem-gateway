package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArt(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// sauceRecord builds the 128-byte metadata trailer the DOS art tools append.
// Layout: "SAUCE" + "00", title(35), author(20), group(20), date(8),
// filesize(4), datatype(1), filetype(1), tinfo1..4(8), comments(1) at offset
// 104, tflags(1), tinfos(22).
func sauceRecord(comments byte) []byte {
	rec := make([]byte, 128)
	copy(rec, "SAUCE00")
	copy(rec[7:], "Some Piece")
	copy(rec[42:], "An Artist")
	rec[104] = comments
	return rec
}

// commentBlock builds the optional COMNT block that sits immediately before a
// SAUCE record, one 64-byte line per comment.
func commentBlock(lines int) []byte {
	b := []byte("COMNT")
	for i := 0; i < lines; i++ {
		line := make([]byte, 64)
		copy(line, "a comment line from the artist")
		b = append(b, line...)
	}
	return b
}

func TestRenderArt(t *testing.T) {
	tests := []struct {
		name    string
		raw     []byte
		want    string
		notWant string
	}{
		{
			name: "strips a bare SAUCE record",
			raw:  append([]byte("\x1b[1;32mART\r\n"), sauceRecord(0)...),
			want: "\x1b[1;32mART\r\n",
			// The record prints as a line of mojibake naming the piece.
			notWant: "SAUCE",
		},
		{
			name: "strips a SAUCE record with its comment block",
			raw: func() []byte {
				b := []byte("ART\r\n")
				b = append(b, commentBlock(2)...)
				return append(b, sauceRecord(2)...)
			}(),
			want:    "ART\r\n",
			notWant: "COMNT",
		},
		{
			name:    "strips the DOS EOF byte the tools write before SAUCE",
			raw:     append([]byte("ART\r\n\x1a"), sauceRecord(0)...),
			want:    "ART\r\n",
			notWant: "\x1a",
		},
		{
			name: "keeps 0x1a when it is art, not an EOF marker",
			// 0x1A is a perfectly good CP437 glyph — a right arrow — so a file
			// with no SAUCE trailer must not be truncated at the first one.
			raw:  []byte("ARROW\x1aHERE\r\n"),
			want: "ARROW\x1aHERE\r\n",
		},
		{
			name: "turns a lone line feed into CRLF",
			// Without the carriage return the art staircases down the screen.
			raw:  []byte("one\ntwo\n"),
			want: "one\r\ntwo\r\n",
		},
		{
			name: "leaves an existing CRLF alone",
			raw:  []byte("one\r\ntwo\r\n"),
			want: "one\r\ntwo\r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderArt(tc.raw)

			if !strings.Contains(got, tc.want) {
				t.Errorf("rendered art missing %q\ngot: %q", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("rendered art still contains %q\ngot: %q", tc.notWant, got)
			}
		})
	}
}

func TestRenderArt_LeavesTheTerminalUsable(t *testing.T) {
	// Art assumes it owns a cleared screen, and it routinely ends mid-colour —
	// without a reset the OK prompt that follows inherits whatever the last
	// escape set.
	got := renderArt([]byte("\x1b[41;33mART"))

	if !strings.HasPrefix(got, "\x1b[0m\x1b[2J\x1b[H") {
		t.Errorf("art should clear the screen before drawing, got %q", got)
	}
	if !strings.Contains(got, "\x1b[0m\x1b[999") {
		t.Errorf("art should reset colour after drawing, got %q", got)
	}
}

func TestRenderArt_ParksTheCursorClearOfTheResultCode(t *testing.T) {
	// A full-screen piece fills the grid to the last cell, and the "\r\nOK\r\n"
	// written straight after it would scroll the terminal twice — the top two
	// rows of the art gone before the caller sees them. Parking two rows short
	// of the bottom lets the result code land without scrolling. ESC[999;1H
	// clamps to whatever height the client actually has.
	got := renderArt([]byte("ART"))

	if !strings.HasSuffix(got, "\x1b[999;1H\x1b[2A") {
		t.Errorf("art should park the cursor two rows off the bottom, got %q", got)
	}
}

func TestBannerArt_Pick(t *testing.T) {
	t.Run("draws a file from the mounted directory", func(t *testing.T) {
		dir := t.TempDir()
		writeArt(t, dir, "welcome.ans", []byte("\x1b[1;35mWELCOME\r\n"))

		got := newBannerArt(dir).pick()

		if !strings.Contains(got, "\x1b[1;35mWELCOME") {
			t.Errorf("expected the art in the banner, got %q", got)
		}
	})

	t.Run("matches .ANS whatever the case, ignores everything else", func(t *testing.T) {
		dir := t.TempDir()
		writeArt(t, dir, "LOUD.ANS", []byte("SHOUTED"))
		writeArt(t, dir, "notes.txt", []byte("NOT ART"))
		writeArt(t, dir, ".hidden.ans", []byte("HIDDEN"))

		for i := 0; i < 20; i++ {
			got := newBannerArt(dir).pick()
			if !strings.Contains(got, "SHOUTED") {
				t.Fatalf("expected only LOUD.ANS to be drawable, got %q", got)
			}
		}
	})

	t.Run("spreads across the directory rather than always drawing one file", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"a.ans", "b.ans", "c.ans"} {
			writeArt(t, dir, n, []byte("art-"+n))
		}
		art := newBannerArt(dir)

		seen := map[string]bool{}
		for i := 0; i < 200; i++ {
			seen[art.pick()] = true
		}

		if len(seen) != 3 {
			t.Errorf("expected all 3 files to come up over 200 picks, saw %d", len(seen))
		}
	})

	t.Run("picks up art dropped in after the first scan", func(t *testing.T) {
		// The whole point of mounting the directory is changing it under a
		// running gateway, so a restart must not be needed.
		dir := t.TempDir()
		writeArt(t, dir, "first.ans", []byte("FIRST"))
		art := newBannerArt(dir)
		if got := art.pick(); !strings.Contains(got, "FIRST") {
			t.Fatalf("precondition: expected FIRST, got %q", got)
		}

		writeArt(t, dir, "second.ans", []byte("SECOND"))

		seen := map[bool]bool{}
		for i := 0; i < 200; i++ {
			seen[strings.Contains(art.pick(), "SECOND")] = true
		}
		if !seen[true] {
			t.Error("art added after the first scan was never drawn")
		}
	})

	t.Run("skips a file too big to blast down a modem session", func(t *testing.T) {
		dir := t.TempDir()
		writeArt(t, dir, "huge.ans", make([]byte, maxBannerArtBytes+1))

		if got := newBannerArt(dir).pick(); got != "" {
			t.Errorf("expected no art for an oversized file, got %d bytes", len(got))
		}
	})
}

func TestBannerFor_FallsBackToTheBuiltInBanner(t *testing.T) {
	builtIn := buildBanner("9.9.9")

	emptyDir := t.TempDir()
	noArtDir := t.TempDir()
	writeArt(t, noArtDir, "readme.md", []byte("drop .ans files here"))

	tests := []struct {
		name string
		art  *bannerArt
	}{
		{"no directory configured", newBannerArt("")},
		{"directory does not exist", newBannerArt(filepath.Join(t.TempDir(), "absent"))},
		{"path is a file, not a directory", newBannerArt(writeArt(t, t.TempDir(), "afile", []byte("x")))},
		{"directory is empty", newBannerArt(emptyDir)},
		{"directory holds no .ans files", newBannerArt(noArtDir)},
		{"no art source at all", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bannerFor(tc.art, "9.9.9"); got != builtIn {
				t.Errorf("expected the built-in banner, got %q", got)
			}
		})
	}
}
