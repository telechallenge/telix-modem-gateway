package config

import (
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Logging   LoggingConfig    `yaml:"logging"`
	Phonebook []PhonebookEntry `yaml:"phonebook"`
	RateLimit RateLimitConfig  `yaml:"rate_limit"`
	Dialer    DialerConfig     `yaml:"dialer"`
	Version   string           `yaml:"-"` // Set at runtime, not from YAML
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
	Number           string           `yaml:"number"`
	Host             string           `yaml:"host"`
	Port             int              `yaml:"port"`
	Name             string           `yaml:"name"`
	RequiredInit     string           `yaml:"required_init,omitempty"` // deprecated, use required_settings.init
	RequiredSettings RequiredSettings `yaml:"required_settings,omitempty"`
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
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

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
		if entry.Host == "" {
			return fmt.Errorf("phonebook[%d]: host is required", i)
		}
		if entry.Port < 1 || entry.Port > 65535 {
			return fmt.Errorf("phonebook[%d]: port must be 1-65535, got %d", i, entry.Port)
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
