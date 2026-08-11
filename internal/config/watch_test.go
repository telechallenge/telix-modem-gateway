package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validYAML = `
server:
  port: 2323
phonebook:
  - number: "5551234"
    host: "bbs.example.com"
    port: 23
    name: "First"
`

const secondEntryYAML = `
server:
  port: 2323
phonebook:
  - number: "5551234"
    host: "bbs.example.com"
    port: 23
    name: "First"
  - number: "5555678"
    host: "other.example.com"
    port: 23
    name: "Second"
`

// writeAtomic saves via write-then-rename, the way vim and sed -i do. Every
// test uses it so the suite exercises the same write shape production sees.
func writeAtomic(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

// newTestWatcher returns a watcher over a fresh config file plus the slices its
// callbacks append to. Polls are driven directly rather than by the ticker, so
// the assertions are about behaviour and not about timing.
func newTestWatcher(t *testing.T) (*Watcher, string, *[]*Config, *[]error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "telix.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load seed config: %v", err)
	}

	reloads := &[]*Config{}
	errs := &[]error{}
	w := NewWatcher(path, cfg,
		func(c *Config) { *reloads = append(*reloads, c) },
		func(e error) { *errs = append(*errs, e) },
	)
	return w, path, reloads, errs
}

func TestWatcher_AppliesValidEditAndRejectsBrokenOne(t *testing.T) {
	t.Run("unchanged file produces no reload", func(t *testing.T) {
		w, _, reloads, errs := newTestWatcher(t)

		w.poll()
		w.poll()

		if len(*reloads) != 0 {
			t.Errorf("expected no reload for unchanged file, got %d", len(*reloads))
		}
		if len(*errs) != 0 {
			t.Errorf("expected no errors, got %v", *errs)
		}
	})

	t.Run("edited file reloads with the new phonebook", func(t *testing.T) {
		w, path, reloads, errs := newTestWatcher(t)

		writeAtomic(t, path, secondEntryYAML)
		w.poll()

		if len(*reloads) != 1 {
			t.Fatalf("expected 1 reload, got %d (errors: %v)", len(*reloads), *errs)
		}
		got := (*reloads)[0]
		if len(got.Phonebook) != 2 {
			t.Fatalf("expected 2 phonebook entries, got %d", len(got.Phonebook))
		}
		// The number added by the edit must be dialable through the new config,
		// which is the whole point of the feature.
		if entry := got.LookupNumber("555-5678"); entry == nil || entry.Name != "Second" {
			t.Errorf("expected new entry reachable by lookup, got %+v", entry)
		}
	})

	t.Run("invalid edit is reported and never applied", func(t *testing.T) {
		w, path, reloads, errs := newTestWatcher(t)

		// Valid YAML, invalid config: a non-busy entry with no host.
		writeAtomic(t, path, "phonebook:\n  - number: \"5551234\"\n    port: 23\n")
		w.poll()

		if len(*reloads) != 0 {
			t.Errorf("a config that fails validation must not be applied, got %d reloads", len(*reloads))
		}
		if len(*errs) != 1 {
			t.Fatalf("expected 1 error, got %d", len(*errs))
		}
	})

	t.Run("a broken file reports once, not once per poll", func(t *testing.T) {
		w, path, _, errs := newTestWatcher(t)

		writeAtomic(t, path, "phonebook: [oh no\n")
		w.poll()
		w.poll()
		w.poll()

		if len(*errs) != 1 {
			t.Errorf("expected 1 error for 3 polls of the same broken file, got %d", len(*errs))
		}
	})

	// The property that makes polling safe against a save in progress: a
	// truncated file is rejected, and the completed save that follows is still
	// picked up rather than being suppressed as already-seen.
	t.Run("partial write is rejected then the completed write applies", func(t *testing.T) {
		w, path, reloads, errs := newTestWatcher(t)

		if err := os.WriteFile(path, []byte("phonebook:\n  - number: \"555"), 0600); err != nil {
			t.Fatalf("partial write: %v", err)
		}
		w.poll()
		if len(*reloads) != 0 {
			t.Fatalf("partial write must not be applied, got %d reloads", len(*reloads))
		}
		if len(*errs) != 1 {
			t.Fatalf("expected the partial write to be reported, got %d errors", len(*errs))
		}

		if err := os.WriteFile(path, []byte(secondEntryYAML), 0600); err != nil {
			t.Fatalf("completed write: %v", err)
		}
		w.poll()

		if len(*reloads) != 1 {
			t.Fatalf("completed write must be applied, got %d reloads (errors: %v)", len(*reloads), *errs)
		}
		if len((*reloads)[0].Phonebook) != 2 {
			t.Errorf("expected the completed config, got %d entries", len((*reloads)[0].Phonebook))
		}
	})

	t.Run("a missing file is reported without stopping the watcher", func(t *testing.T) {
		w, path, reloads, errs := newTestWatcher(t)

		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
		w.poll()
		w.poll()

		if len(*errs) != 1 {
			t.Errorf("expected 1 error for a missing file across 2 polls, got %d", len(*errs))
		}

		// The file coming back must still reload — the watcher has to survive
		// the gap a non-atomic save leaves.
		writeAtomic(t, path, secondEntryYAML)
		w.poll()
		if len(*reloads) != 1 {
			t.Errorf("expected reload after the file returned, got %d", len(*reloads))
		}
	})
}

// The ticker path, end to end. Delivery is taken off a channel rather than a
// shared slice so the handoff between the watcher's goroutine and the test is
// synchronised — the assertion is about the reload arriving, not about winning
// a read against the goroutine that produced it.
func TestWatcher_StartStopDeliversReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telix.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load seed config: %v", err)
	}

	reloads := make(chan *Config, 4)
	w := NewWatcher(path, cfg,
		func(c *Config) { reloads <- c },
		func(e error) { t.Errorf("unexpected watcher error: %v", e) },
	)
	w.interval = 10 * time.Millisecond
	w.Start()
	defer w.Stop()

	writeAtomic(t, path, secondEntryYAML)

	select {
	case got := <-reloads:
		if len(got.Phonebook) != 2 {
			t.Errorf("expected the edited phonebook, got %d entries", len(got.Phonebook))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not deliver a reload within 5s")
	}
}

func TestReloadConfig_IntervalDefaultsAndFloor(t *testing.T) {
	tests := []struct {
		name     string
		interval int
		want     time.Duration
	}{
		{"unset defaults to 2s", 0, 2 * time.Second},
		{"below the floor is clamped", 0, 2 * time.Second},
		{"sub-second is clamped to the floor", -5, 2 * time.Second},
		{"honours a larger value", 30, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReloadConfig{Interval: tt.interval}
			if got := r.GetInterval(); got != tt.want {
				t.Errorf("GetInterval() = %v, want %v", got, tt.want)
			}
		})
	}

	if got := (&ReloadConfig{Interval: 1}).GetInterval(); got != MinReloadInterval {
		t.Errorf("interval of 1s should be the floor, got %v", got)
	}
}

func TestLoad_ReloadDefaultsToEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telix.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Reload.Enabled {
		t.Error("reload should default to enabled when the section is absent")
	}

	// And an explicit opt-out must survive the defaulting.
	writeAtomic(t, path, validYAML+"\nreload:\n  enabled: false\n")
	off, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if off.Reload.Enabled {
		t.Error("reload.enabled: false must disable the watcher")
	}
}
