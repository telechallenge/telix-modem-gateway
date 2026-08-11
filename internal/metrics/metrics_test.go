package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gather renders the registry the way Prometheus would scrape it, so the
// assertions below read the same text a scrape produces rather than poking at
// collector internals.
func gather(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading scrape body: %v", err)
	}
	return string(body)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("scrape output missing %q\n--- got ---\n%s", w, body)
		}
	}
}

func TestMetrics_RecordedActivityAppearsInScrape(t *testing.T) {
	tests := []struct {
		name   string
		record func(m *Metrics)
		want   []string
	}{
		{
			name: "build info carries the version label",
			record: func(m *Metrics) {
				m.SetBuildInfo("v1.2.3")
			},
			want: []string{`telix_build_info{version="v1.2.3"} 1`},
		},
		{
			name: "a finished session leaves the active gauge at zero but counts the total",
			record: func(m *Metrics) {
				m.SessionStarted()
				m.SessionStarted()
				m.SessionEnded(2 * time.Second)
				m.SessionEnded(3 * time.Second)
			},
			want: []string{
				"telix_sessions_active 0",
				"telix_sessions_total 2",
				"telix_session_duration_seconds_count 2",
				"telix_session_duration_seconds_sum 5",
			},
		},
		{
			name: "an in-flight session keeps the active gauge raised",
			record: func(m *Metrics) {
				m.SessionStarted()
				m.SessionStarted()
				m.SessionEnded(time.Second)
			},
			want: []string{
				"telix_sessions_active 1",
				"telix_sessions_total 2",
			},
		},
		{
			name: "rejections are broken out by reason",
			record: func(m *Metrics) {
				m.ConnectionRejected(ReasonRateLimited)
				m.ConnectionRejected(ReasonRateLimited)
				m.ConnectionRejected(ReasonLimitExceeded)
			},
			want: []string{
				`telix_connections_rejected_total{reason="rate_limited"} 2`,
				`telix_connections_rejected_total{reason="limit_exceeded"} 1`,
			},
		},
		{
			name: "dials are broken out by outcome and destination",
			record: func(m *Metrics) {
				m.DialCompleted("warez", OutcomeSuccess, 1500*time.Millisecond)
				m.DialCompleted("warez", OutcomeNoCarrier, 500*time.Millisecond)
				m.DialCompleted("unknown", OutcomeUnknownNumber, 0)
			},
			want: []string{
				`telix_dials_total{entry="warez",outcome="success"} 1`,
				`telix_dials_total{entry="warez",outcome="no_carrier"} 1`,
				`telix_dials_total{entry="unknown",outcome="unknown_number"} 1`,
				"telix_dial_duration_seconds_count 3",
			},
		},
		{
			name: "bytes are counted per direction",
			record: func(m *Metrics) {
				m.BytesTransferred(DirectionToRemote, 100)
				m.BytesTransferred(DirectionToRemote, 40)
				m.BytesTransferred(DirectionToClient, 7)
			},
			want: []string{
				`telix_bytes_total{direction="to_remote"} 140`,
				`telix_bytes_total{direction="to_client"} 7`,
			},
		},
		{
			name: "a reachability probe publishes the verdict, its cost and when it ran",
			record: func(m *Metrics) {
				m.BBSProbed("Telehack", "telehack.com:23", true, 120*time.Millisecond)
				m.BBSProbed("Dead BBS", "192.0.2.1:23", false, 5*time.Second)
			},
			want: []string{
				`telix_bbs_reachable{address="telehack.com:23",entry="Telehack"} 1`,
				`telix_bbs_reachable{address="192.0.2.1:23",entry="Dead BBS"} 0`,
				`telix_bbs_probe_duration_seconds{address="telehack.com:23",entry="Telehack"} 0.12`,
				`telix_bbs_probe_duration_seconds{address="192.0.2.1:23",entry="Dead BBS"} 5`,
				`telix_bbs_last_probe_timestamp_seconds{address="telehack.com:23",entry="Telehack"}`,
			},
		},
		{
			name: "a board that comes back up flips its gauge rather than adding a series",
			record: func(m *Metrics) {
				m.BBSProbed("Fungus Land", "10.0.0.1:8401", false, time.Second)
				m.BBSProbed("Fungus Land", "10.0.0.1:8401", true, 10*time.Millisecond)
			},
			want: []string{`telix_bbs_reachable{address="10.0.0.1:8401",entry="Fungus Land"} 1`},
		},
		{
			name: "an active dial is tracked while it is in flight",
			record: func(m *Metrics) {
				m.DialStarted()
				m.DialStarted()
				m.DialCompleted("bbs", OutcomeSuccess, time.Second)
			},
			want: []string{"telix_dials_in_flight 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			tt.record(m)
			mustContain(t, gather(t, m), tt.want...)
		})
	}
}

// The Go runtime and process collectors are a large part of why client_golang
// was chosen over hand-rolling the exposition format; if registration ever
// silently stops including them the dashboard's memory and goroutine panels go
// blank, so assert they are present.
func TestMetrics_IncludesRuntimeCollectors(t *testing.T) {
	m := New()
	mustContain(t, gather(t, m), "go_goroutines", "go_memstats_alloc_bytes", "process_open_fds")
}

// Two Metrics instances must not fight over a shared global registry — the
// server creates one per process, but tests create many.
func TestMetrics_InstancesAreIndependent(t *testing.T) {
	a, b := New(), New()
	a.SessionStarted()

	mustContain(t, gather(t, a), "telix_sessions_active 1")
	mustContain(t, gather(t, b), "telix_sessions_active 0")
}

func TestMetrics_ServerExposesMetricsOverHTTP(t *testing.T) {
	m := New()
	m.SetBuildInfo("v9.9.9")

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), `telix_build_info{version="v9.9.9"} 1`) {
		t.Errorf("served metrics missing build info:\n%s", body)
	}
}

// The reachability gauges only cover lines the prober dials, so a phonebook of
// always-busy decoys is invisible to them — the dashboard silently showed
// fewer boards than the phone list held. The inventory series is what accounts
// for the rest: every entry appears exactly once, carrying whether it is busy,
// whether or not anything ever probes it.
func TestMetrics_PhonebookInventoryCoversBusyAndDialableLines(t *testing.T) {
	m := New()

	m.PhonebookLine("Terminal Velocity", "host.docker.internal:22023", false)
	m.PhonebookLine("Recipe Box", "host.docker.internal:22023", true)
	m.PhonebookLine("", "", true) // unnamed, and busy entries need no host

	body := gather(t, m)
	mustContain(t, body,
		`telix_phonebook_entry{address="host.docker.internal:22023",busy="false",entry="Terminal Velocity"} 1`,
		`telix_phonebook_entry{address="host.docker.internal:22023",busy="true",entry="Recipe Box"} 1`,
		`telix_phonebook_entry{address="",busy="true",entry="unnamed"} 1`,
	)
}

// A nil *Metrics is what every call site holds when metrics are disabled in
// config, so every method must tolerate it rather than forcing nil checks (and
// the panics a missed one would cause) into the session hot path.
func TestMetrics_NilReceiverIsSafe(t *testing.T) {
	var m *Metrics // disabled

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil *Metrics panicked: %v", r)
		}
	}()

	m.SetBuildInfo("v1")
	m.SessionStarted()
	m.SessionEnded(time.Second)
	m.ConnectionRejected(ReasonRateLimited)
	m.DialStarted()
	m.DialCompleted("x", OutcomeSuccess, time.Second)
	m.BytesTransferred(DirectionToClient, 10)
	m.BBSProbed("x", "h:23", true, time.Second)
	m.PhonebookLine("x", "h:23", false)
}
