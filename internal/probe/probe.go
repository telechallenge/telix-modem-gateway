// Package probe periodically checks that the phonebook's boards are answering.
//
// The dial counters say what happened when somebody called; this says whether a
// call would connect right now. Nothing else in the stack can: a BBS in a
// container shows up in the Docker metrics whether or not its listener is
// healthy, and a board on somebody else's host is invisible until a caller hits
// it and gets NO CARRIER.
//
// What a probe proves is exactly one thing — the endpoint completed a TCP
// handshake. It does not log in, does not negotiate telnet (see dialer.Probe)
// and cannot tell a working board from a listener that accepts and then hangs.
package probe

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"telix/internal/config"
	"telix/internal/dialer"
	"telix/internal/logging"
	"telix/internal/metrics"
)

// maxConcurrent bounds how many boards are dialled at once. The phonebook is
// operator-controlled and can be long; a cycle that opened every socket at once
// would arrive at the boards as a burst and, on a shared host, look like a scan.
const maxConcurrent = 8

// target is one endpoint to probe, and the phonebook entries that point at it.
// Several numbers commonly reach the same board — a primary and a priority
// line, say — and each wants its own series while the board wants one knock.
type target struct {
	host    string
	port    int
	address string
	entries []string
}

// Prober runs the reachability cycle. Its zero value is not usable; call New.
type Prober struct {
	targets  []target
	dialer   *dialer.Dialer
	metrics  *metrics.Metrics
	logger   *logging.Logger
	interval time.Duration
	enabled  bool

	stop chan struct{}
	done chan struct{}

	// reachable remembers the last verdict per address so only changes are
	// logged. At one line per board per cycle a 20-entry phonebook would write
	// 28,800 lines a day and bury everything else in the log.
	mu        sync.Mutex
	reachable map[string]bool
}

// New builds a Prober over cfg's phonebook. Busy entries are left out: they
// never place an outbound call, so their host is not on any path a caller can
// take — and config does not require them to name one at all.
func New(cfg *config.Config, logger *logging.Logger, m *metrics.Metrics) *Prober {
	p := &Prober{
		dialer:    dialer.New(cfg.Probe.GetTimeout(), cfg.Dialer.ParsedNetworks()),
		metrics:   m,
		logger:    logger,
		interval:  cfg.Probe.GetInterval(),
		enabled:   cfg.Probe.Enabled,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		reachable: make(map[string]bool),
	}

	byAddress := make(map[string]int) // address → index into p.targets
	for _, entry := range cfg.Phonebook {
		if entry.Busy || entry.Host == "" {
			continue
		}
		address := net.JoinHostPort(entry.Host, strconv.Itoa(entry.Port))
		name := entry.Name
		if name == "" {
			// The metric labels a line by name; an unnamed entry would
			// otherwise collapse several boards into one empty-string series.
			name = address
		}
		if i, seen := byAddress[address]; seen {
			p.targets[i].entries = append(p.targets[i].entries, name)
			continue
		}
		byAddress[address] = len(p.targets)
		p.targets = append(p.targets, target{
			host:    entry.Host,
			port:    entry.Port,
			address: address,
			entries: []string{name},
		})
	}

	return p
}

// Start begins probing in the background. The first cycle runs immediately
// rather than after one interval, so the dashboard is populated seconds after a
// restart instead of a minute — but it runs in the goroutine, not inline, since
// an unreachable board would otherwise hold up the telnet listener for the
// length of a timeout.
func (p *Prober) Start() {
	if !p.enabled {
		close(p.done)
		return
	}

	p.logger.Info().
		Str("event", "probe_started").
		Int("targets", len(p.targets)).
		Str("interval", p.interval.String()).
		Msg("")

	go func() {
		defer close(p.done)

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			p.runOnce()
			select {
			case <-p.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop ends the loop and waits for the cycle in flight. Safe on a disabled
// prober, so main does not have to guard the call.
func (p *Prober) Stop() {
	close(p.stop)
	<-p.done
}

// runOnce probes every target and records the results. Cycles never overlap:
// the loop calls this synchronously, and a Ticker drops ticks that arrive while
// the receiver is busy, so a cycle running longer than the interval delays the
// next one rather than stacking a second copy on top of it.
func (p *Prober) runOnce() {
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup

	for _, t := range p.targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p.probe(t)
		}(t)
	}

	wg.Wait()
}

func (p *Prober) probe(t target) {
	start := time.Now()
	err := p.dialer.Probe(t.host, t.port)
	elapsed := time.Since(start)

	for _, entry := range t.entries {
		p.metrics.BBSProbed(entry, t.address, err == nil, elapsed)
	}
	p.logChange(t, err, elapsed)
}

// logChange writes a line only when a board's verdict differs from the last
// cycle's, including the first one — the transitions are the news, and they are
// what a log reader wants to correlate against a caller's complaint.
func (p *Prober) logChange(t target, err error, elapsed time.Duration) {
	up := err == nil

	p.mu.Lock()
	was, known := p.reachable[t.address]
	p.reachable[t.address] = up
	p.mu.Unlock()

	if known && was == up {
		return
	}

	if up {
		p.logger.Info().
			Str("event", "probe_reachable").
			Str("address", t.address).
			Str("entries", strings.Join(t.entries, ", ")).
			Int("duration_ms", int(elapsed.Milliseconds())).
			Msg("")
		return
	}

	p.logger.Warn().
		Str("event", "probe_unreachable").
		Str("address", t.address).
		Str("entries", strings.Join(t.entries, ", ")).
		Int("duration_ms", int(elapsed.Milliseconds())).
		Err(err).
		Msg("")
}
