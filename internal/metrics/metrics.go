// Package metrics exposes gateway telemetry in Prometheus exposition format.
//
// One Metrics value owns its own registry rather than using the client_golang
// default, so tests can build instances freely without colliding on global
// state and a second gateway in one process would stay separable.
//
// Every method tolerates a nil receiver. Metrics are optional (see
// config.MetricsConfig), and a disabled gateway holds a nil *Metrics — making
// nil a no-op keeps `if s.metrics != nil` out of the session hot path, where a
// single missed check would be a panic on a live connection.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Rejection reasons for telix_connections_rejected_total. These are the two
// gates in Server.handleConnection, kept as constants so the label values can
// never drift apart from the dashboard queries that select on them.
const (
	ReasonRateLimited   = "rate_limited"
	ReasonLimitExceeded = "limit_exceeded"
)

// Dial outcomes for telix_dials_total. Bounded set — never derive a label from
// user input, or a dialled number becomes unbounded cardinality.
const (
	OutcomeSuccess          = "success"
	OutcomeNoCarrier        = "no_carrier"
	OutcomeBusy             = "busy"
	OutcomeTimeout          = "timeout"
	OutcomeUnknownNumber    = "unknown_number"
	OutcomeAuthFailed       = "auth_failed"
	OutcomeSettingsMismatch = "settings_mismatch"
	OutcomeBlocked          = "blocked"
)

// Byte-counter directions, relative to the gateway.
const (
	DirectionToRemote = "to_remote" // client → BBS
	DirectionToClient = "to_client" // BBS → client
)

// Metrics holds the gateway's collectors and the registry they belong to.
type Metrics struct {
	registry *prometheus.Registry

	buildInfo *prometheus.GaugeVec

	sessionsActive   prometheus.Gauge
	sessionsTotal    prometheus.Counter
	sessionDuration  prometheus.Histogram
	connectionsRejct *prometheus.CounterVec

	dialsInFlight prometheus.Gauge
	dialsTotal    *prometheus.CounterVec
	dialDuration  prometheus.Histogram

	bytesTotal *prometheus.CounterVec
}

// New builds a Metrics with its own registry, pre-registered with the Go
// runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "telix_build_info",
			Help: "Build information. Always 1; the version label carries the value.",
		}, []string{"version"}),

		sessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telix_sessions_active",
			Help: "Modem sessions currently connected.",
		}),
		sessionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telix_sessions_total",
			Help: "Modem sessions accepted since start.",
		}),
		// A BBS call runs from seconds to hours, so the buckets span that
		// rather than the sub-second defaults, which would pile every real
		// session into +Inf and make the histogram useless.
		sessionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "telix_session_duration_seconds",
			Help:    "Time from accept to disconnect, per session.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200},
		}),
		connectionsRejct: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telix_connections_rejected_total",
			Help: "Connections refused before a session was created.",
		}, []string{"reason"}),

		dialsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telix_dials_in_flight",
			Help: "Outbound dials currently being placed.",
		}),
		dialsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telix_dials_total",
			Help: "Completed dial attempts by phonebook entry and outcome.",
		}, []string{"entry", "outcome"}),
		// Dial time is dominated by the simulated ring cadence plus the TCP
		// connect, so a few seconds is normal and the tail is the timeout.
		dialDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "telix_dial_duration_seconds",
			Help:    "Time from ATDT to CONNECT or failure.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60},
		}),

		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telix_bytes_total",
			Help: "Bytes bridged in data mode, by direction.",
		}, []string{"direction"}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.buildInfo,
		m.sessionsActive,
		m.sessionsTotal,
		m.sessionDuration,
		m.connectionsRejct,
		m.dialsInFlight,
		m.dialsTotal,
		m.dialDuration,
		m.bytesTotal,
	)

	// Pre-create the rejection series so a gateway that has never refused a
	// connection reports 0 rather than no series at all — an absent series
	// makes rate() return nothing and the panel read "No data" instead of a
	// flat zero line.
	for _, reason := range []string{ReasonRateLimited, ReasonLimitExceeded} {
		m.connectionsRejct.WithLabelValues(reason)
	}
	for _, dir := range []string{DirectionToRemote, DirectionToClient} {
		m.bytesTotal.WithLabelValues(dir)
	}

	return m
}

// Handler serves the registry in Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A scrape must never take the gateway down with it.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// SetBuildInfo records the running version.
func (m *Metrics) SetBuildInfo(version string) {
	if m == nil {
		return
	}
	m.buildInfo.WithLabelValues(version).Set(1)
}

// SessionStarted records an accepted session.
func (m *Metrics) SessionStarted() {
	if m == nil {
		return
	}
	m.sessionsActive.Inc()
	m.sessionsTotal.Inc()
}

// SessionEnded records a disconnect and how long the session lasted.
func (m *Metrics) SessionEnded(d time.Duration) {
	if m == nil {
		return
	}
	m.sessionsActive.Dec()
	m.sessionDuration.Observe(d.Seconds())
}

// ConnectionRejected records a connection refused before a session existed.
// reason must be one of the Reason* constants.
func (m *Metrics) ConnectionRejected(reason string) {
	if m == nil {
		return
	}
	m.connectionsRejct.WithLabelValues(reason).Inc()
}

// DialStarted records the beginning of an outbound dial.
func (m *Metrics) DialStarted() {
	if m == nil {
		return
	}
	m.dialsInFlight.Inc()
}

// DialCompleted records a finished dial. entry is the phonebook entry name (or
// "unknown" for a number not in the book) — never the raw dialled digits, which
// would be unbounded cardinality. outcome must be one of the Outcome* constants.
func (m *Metrics) DialCompleted(entry, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.dialsInFlight.Dec()
	m.dialsTotal.WithLabelValues(entry, outcome).Inc()
	m.dialDuration.Observe(d.Seconds())
}

// BytesTransferred records bytes bridged in data mode. direction must be one of
// the Direction* constants.
func (m *Metrics) BytesTransferred(direction string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.bytesTotal.WithLabelValues(direction).Add(float64(n))
}
