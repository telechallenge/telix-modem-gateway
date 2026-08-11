package config

import (
	"crypto/sha256"
	"os"
	"time"
)

// Watcher polls the config file and hands back a parsed, validated config each
// time its contents change.
//
// It compares a content hash rather than mtime, for two reasons. Timestamps on
// Linux come from a coarse cached clock — the same trap that makes an
// mtime-keyed banner cache silently miss a file dropped in within the same tick
// — and a hash additionally treats "edited and reverted" as no change at all,
// which mtime cannot. The file is a few kilobytes, so hashing it is cheaper
// than the syscall that read it.
//
// The watcher never applies a config it could not parse and validate. That is
// what makes polling safe against a partial write: a file caught mid-save is
// rejected, and because the completed save hashes differently it is picked up
// on the very next poll rather than being remembered as already-seen.
type Watcher struct {
	path     string
	interval time.Duration

	// onReload receives each validated config. onError receives every reason a
	// poll produced nothing — a read failure, a parse failure, a validation
	// failure. Both are called from the watcher's own goroutine, one at a time.
	onReload func(*Config)
	onError  func(error)

	// lastHash is the digest of the bytes most recently *considered*, whether
	// they were applied or rejected. Recording rejects too is what stops a
	// permanently broken file from re-reporting itself every poll; recording
	// them under their own digest is what stops that suppression from also
	// swallowing the operator's fix.
	lastHash [sha256.Size]byte
	lastErr  string

	stop chan struct{}
	done chan struct{}
}

// NewWatcher builds a watcher over the file cfg was loaded from. It is seeded
// from cfg's own source digest, so the running config is the baseline and the
// first poll reports only genuine edits.
func NewWatcher(path string, cfg *Config, onReload func(*Config), onError func(error)) *Watcher {
	return &Watcher{
		path:     path,
		interval: cfg.Reload.GetInterval(),
		onReload: onReload,
		onError:  onError,
		lastHash: cfg.SourceHash(),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins polling in the background. A disabled watcher starts nothing but
// is still safe to Stop, so the caller needs no guard.
func (w *Watcher) Start() {
	go func() {
		defer close(w.done)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
				w.poll()
			}
		}
	}()
}

// Stop ends the loop and waits for any poll in flight, so a reload cannot land
// after shutdown has begun.
func (w *Watcher) Stop() {
	close(w.stop)
	<-w.done
}

// poll reads the file once and applies it if it is both new and valid.
func (w *Watcher) poll() {
	data, err := os.ReadFile(w.path)
	if err != nil {
		// A read error has no digest to dedupe on — the file may be absent
		// mid-rename — so dedupe on the message instead.
		if err.Error() != w.lastErr {
			w.lastErr = err.Error()
			w.onError(err)
		}
		return
	}
	w.lastErr = ""

	hash := sha256.Sum256(data)
	if hash == w.lastHash {
		return
	}
	// Recorded before the outcome is known: a file that fails to parse must not
	// re-report itself on every poll for as long as it stays broken.
	w.lastHash = hash

	cfg, err := Load(w.path)
	if err != nil {
		w.onError(err)
		return
	}

	// Load re-read the file, so it may have caught a later write than the one
	// just hashed. Track what was actually applied, or that write is treated as
	// already-seen and never applied.
	w.lastHash = cfg.SourceHash()

	w.onReload(cfg)
}
