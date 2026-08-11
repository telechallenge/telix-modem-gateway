package config

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Logging   LoggingConfig    `yaml:"logging"`
	Phonebook []PhonebookEntry `yaml:"phonebook"`
	RateLimit RateLimitConfig  `yaml:"rate_limit"`
	Dialer    DialerConfig     `yaml:"dialer"`
	Metrics   MetricsConfig    `yaml:"metrics"`
	Probe     ProbeConfig      `yaml:"probe"`
	Reload    ReloadConfig     `yaml:"reload"`
	Version   string           `yaml:"-"` // Set at runtime, not from YAML

	// sourceHash is the digest of the bytes this config was parsed from. The
	// watcher seeds itself from it rather than re-reading the file, so the
	// baseline is exactly the content now running — re-reading would leave a
	// window in which an edit landing between load and watch start is adopted
	// as the baseline and therefore never applied.
	sourceHash [sha256.Size]byte
}

// SourceHash returns the digest of the file this config was loaded from.
func (c *Config) SourceHash() [sha256.Size]byte {
	return c.sourceHash
}

// MinProbeInterval is the floor under probe.interval. Several phonebook hosts
// belong to other people, and a probe is a connection on their board however
// briefly it lasts; a sub-10s interval is abuse rather than monitoring. Same
// reasoning as S12's minimum guard time — the setting is clamped, not obeyed.
const MinProbeInterval = 10 * time.Second

// ProbeConfig controls the periodic reachability check over the phonebook. It
// answers the one question the dial counters cannot: is the board accepting
// connections *now*, whether or not anybody has called it.
//
// There is no default-on: enabling this makes the gateway open a TCP connection
// to every non-busy phonebook host on a timer, several of which are run by
// other people, so an upgrade must not start doing it unasked. An absent
// section leaves probing off, the same contract the metrics section has.
type ProbeConfig struct {
	Enabled  bool `yaml:"enabled"`
	Interval int  `yaml:"interval"` // seconds between cycles; default 60, floor 10
	Timeout  int  `yaml:"timeout"`  // seconds to wait for a connection; default 5
}

// GetInterval returns the gap between probe cycles, defaulted and clamped to
// MinProbeInterval.
func (p *ProbeConfig) GetInterval() time.Duration {
	if p.Interval <= 0 {
		return 60 * time.Second
	}
	if d := time.Duration(p.Interval) * time.Second; d > MinProbeInterval {
		return d
	}
	return MinProbeInterval
}

// GetTimeout returns how long one probe waits for a connection.
func (p *ProbeConfig) GetTimeout() time.Duration {
	if p.Timeout <= 0 {
		return 5 * time.Second
	}
	return time.Duration(p.Timeout) * time.Second
}

// MinReloadInterval floors reload.interval. The poll costs one read and one
// hash of a small file, so a tighter setting buys nothing an operator can
// perceive while adding wakeups for the life of the process. Clamped, not
// obeyed — the same contract probe.interval has.
const MinReloadInterval = time.Second

// ReloadConfig controls watching the config file for edits. Unlike probe, this
// defaults to ON: it reads one local file the operator already controls and has
// no effect anybody else can observe, so there is nothing for an upgrade to
// start doing unasked.
//
// A change is adopted only if it parses AND validates; a file caught mid-write
// fails one of those and is retried on the next poll, so the running gateway is
// never taken down by a half-saved edit.
type ReloadConfig struct {
	Enabled  bool `yaml:"enabled"`
	Interval int  `yaml:"interval"` // seconds between polls; default 2, floor 1
}

// GetInterval returns the gap between polls, defaulted and clamped to
// MinReloadInterval.
func (r *ReloadConfig) GetInterval() time.Duration {
	if r.Interval <= 0 {
		return 2 * time.Second
	}
	if d := time.Duration(r.Interval) * time.Second; d > MinReloadInterval {
		return d
	}
	return MinReloadInterval
}

// MetricsConfig controls the Prometheus endpoint. It listens separately from
// the telnet port so the exposition endpoint is never reachable from a modem
// session, and so it can be bound somewhere the telnet listener is not.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Bind    string `yaml:"bind"` // host to bind; empty means all interfaces
}

// GetPort returns the metrics port, defaulting to 9101.
func (m *MetricsConfig) GetPort() int {
	if m.Port <= 0 {
		return 9101
	}
	return m.Port
}

// Addr returns the listen address for the metrics endpoint.
func (m *MetricsConfig) Addr() string {
	return fmt.Sprintf("%s:%d", m.Bind, m.GetPort())
}

type DialerConfig struct {
	AllowedNetworks []string `yaml:"allowed_networks"` // CIDRs like "10.0.0.0/8"
	parsedNetworks  []*net.IPNet
}

// ParsedNetworks returns the parsed CIDR networks. Call after Load().
func (d *DialerConfig) ParsedNetworks() []*net.IPNet {
	return d.parsedNetworks
}

type ServerConfig struct {
	Port           int `yaml:"port"`
	MaxConnections int `yaml:"max_connections"`
	MaxPerIP       int `yaml:"max_per_ip"`
	IdleTimeout    int `yaml:"idle_timeout"` // seconds
	// BannerDir holds ANSI art (.ANS) to greet callers with, one file picked at
	// random per session. Empty, missing or artless falls back to the built-in
	// banner, so the gateway runs unchanged without it.
	BannerDir string `yaml:"banner_dir"`
	// TrustedProxies are CIDRs whose peers are reverse proxies rather than
	// callers — the web terminal's container address, in practice. They are
	// exempt from max_per_ip and from rate_limit, both of which are per-IP and
	// therefore meaningless against one address carrying a whole population of
	// browser users. Global ceilings (max_connections) still apply. Empty by
	// default: nothing is trusted unless it is named here.
	TrustedProxies       []string `yaml:"trusted_proxies"`
	parsedTrustedProxies []*net.IPNet
}

// ParsedTrustedProxies returns the parsed trusted_proxies CIDRs.
func (s *ServerConfig) ParsedTrustedProxies() []*net.IPNet {
	return s.parsedTrustedProxies
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

type RequiredSettings struct {
	Init            string `yaml:"init,omitempty"`
	Baud            int    `yaml:"baud,omitempty"`
	ErrorCorrection *bool  `yaml:"error_correction,omitempty"`
	Compression     *bool  `yaml:"compression,omitempty"`
}

// ValidBaudRates is the set of baud rates supported by AT&N.
var ValidBaudRates = map[int]bool{
	300: true, 1200: true, 2400: true, 4800: true, 7200: true,
	9600: true, 14400: true, 19200: true, 38400: true, 56000: true,
}

type PhonebookEntry struct {
	Number   string `yaml:"number"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	Password string `yaml:"password,omitempty"`
	// Busy marks a line that is always engaged: dialling it reports BUSY and
	// no outbound call is ever placed, so host and port go unused and are not
	// required.
	Busy bool `yaml:"busy,omitempty"`
	// Telnet decides whether 0xFF sent to this board is doubled per RFC 854.
	// "auto" (the default) infers it from whether the board answers negotiation,
	// which is right for anything modern. A board that eats IAC without ever
	// negotiating — a 1990s DOS BBS behind a telnet front end, typically —
	// needs "yes": undoubled, its front end swallows every 0xFF along with the
	// byte behind it, and since ZMODEM cannot escape 0xFF in-band (ZDLE covers
	// only 0x00-0x1F and 0x80-0x9F) that corrupts every binary upload while
	// plain-ASCII ones sail through. "no" forces it off for a genuinely raw
	// TCP peer that the inference somehow misreads.
	Telnet           string           `yaml:"telnet,omitempty"`        // auto (default) | yes | no
	RequiredInit     string           `yaml:"required_init,omitempty"` // deprecated, use required_settings.init
	RequiredSettings RequiredSettings `yaml:"required_settings,omitempty"`
}

// TelnetMode reports the entry's IAC-doubling policy, normalised and defaulted.
func (e PhonebookEntry) TelnetMode() string {
	switch strings.ToLower(strings.TrimSpace(e.Telnet)) {
	case "yes", "no":
		return strings.ToLower(strings.TrimSpace(e.Telnet))
	default:
		return "auto"
	}
}

type RateLimitConfig struct {
	Enabled       bool `yaml:"enabled"`
	MaxAttempts   int  `yaml:"max_attempts"`
	WindowSeconds int  `yaml:"window_seconds"`
	BlockDuration int  `yaml:"block_duration"` // seconds
}

func (c *RateLimitConfig) GetWindow() time.Duration {
	return time.Duration(c.WindowSeconds) * time.Second
}

func (c *RateLimitConfig) GetBlockDuration() time.Duration {
	return time.Duration(c.BlockDuration) * time.Second
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Server: ServerConfig{
			Port:           2323,
			MaxConnections: 100,
			MaxPerIP:       5,
			IdleTimeout:    300,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		RateLimit: RateLimitConfig{
			Enabled:       true,
			MaxAttempts:   5,
			WindowSeconds: 60,
			BlockDuration: 300,
		},
		Reload: ReloadConfig{
			Enabled:  true,
			Interval: 2,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	cfg.sourceHash = sha256.Sum256(data)

	// Backward compat: migrate required_init → required_settings.init
	for i := range cfg.Phonebook {
		if cfg.Phonebook[i].RequiredInit != "" && cfg.Phonebook[i].RequiredSettings.Init == "" {
			cfg.Phonebook[i].RequiredSettings.Init = cfg.Phonebook[i].RequiredInit
		}
	}

	// Apply defaults: error_correction and compression default to true
	for i := range cfg.Phonebook {
		if cfg.Phonebook[i].RequiredSettings.ErrorCorrection == nil {
			cfg.Phonebook[i].RequiredSettings.ErrorCorrection = boolPtr(true)
		}
		if cfg.Phonebook[i].RequiredSettings.Compression == nil {
			cfg.Phonebook[i].RequiredSettings.Compression = boolPtr(true)
		}
	}

	// Parse allowed_networks CIDRs
	for _, cidr := range cfg.Dialer.AllowedNetworks {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("dialer.allowed_networks: invalid CIDR %q: %w", cidr, err)
		}
		cfg.Dialer.parsedNetworks = append(cfg.Dialer.parsedNetworks, ipNet)
	}

	// Parse server.trusted_proxies CIDRs. A typo here would silently reinstate
	// the per-IP limits on the web proxy — the exact failure this setting
	// exists to remove — so it is a startup error, never a skipped entry.
	for _, cidr := range cfg.Server.TrustedProxies {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies: invalid CIDR %q: %w", cidr, err)
		}
		cfg.Server.parsedTrustedProxies = append(cfg.Server.parsedTrustedProxies, ipNet)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server port must be 1-65535, got %d", c.Server.Port)
	}
	if c.Server.MaxConnections < 1 {
		return fmt.Errorf("max_connections must be positive")
	}
	if c.Server.MaxPerIP < 1 {
		return fmt.Errorf("max_per_ip must be positive")
	}
	for i, entry := range c.Phonebook {
		if entry.Number == "" {
			return fmt.Errorf("phonebook[%d]: number is required", i)
		}
		// A busy entry never dials, so there is nothing for host and port to
		// point at — requiring them would force an operator to invent an
		// unreachable host for a number that is only ever engaged.
		if !entry.Busy {
			if entry.Host == "" {
				return fmt.Errorf("phonebook[%d]: host is required", i)
			}
			if entry.Port < 1 || entry.Port > 65535 {
				return fmt.Errorf("phonebook[%d]: port must be 1-65535, got %d", i, entry.Port)
			}
		}
		// Reject a typo rather than silently defaulting it to auto: an operator
		// who wrote `telnet: ture` would otherwise get the inference back with no
		// sign it had been ignored, which is the failure this setting exists for.
		if t := strings.ToLower(strings.TrimSpace(entry.Telnet)); t != "" && t != "auto" && t != "yes" && t != "no" {
			return fmt.Errorf("phonebook[%d]: telnet must be auto, yes or no, got %q", i, entry.Telnet)
		}
		if entry.RequiredSettings.Baud != 0 && !ValidBaudRates[entry.RequiredSettings.Baud] {
			return fmt.Errorf("phonebook[%d]: invalid required baud rate %d", i, entry.RequiredSettings.Baud)
		}
	}
	return nil
}

func (c *Config) LookupNumber(number string) *PhonebookEntry {
	// Normalize number by removing dashes and spaces
	normalized := normalizeNumber(number)
	for i := range c.Phonebook {
		if normalizeNumber(c.Phonebook[i].Number) == normalized {
			return &c.Phonebook[i]
		}
	}
	return nil
}

func boolPtr(b bool) *bool {
	return &b
}

func normalizeNumber(number string) string {
	result := make([]byte, 0, len(number))
	for i := 0; i < len(number); i++ {
		c := number[i]
		switch {
		case c >= '0' && c <= '9':
			result = append(result, c)
		case c == '-', c == '(', c == ')', c == '.', c == '*', c == '#', c == ' ':
			// Valid separator/special chars, skip during normalization
		default:
			// Unexpected character, return empty to reject the match
			return ""
		}
	}
	return string(result)
}
