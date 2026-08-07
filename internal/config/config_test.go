package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNumber(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"916-555-1212", "9165551212"},
		{"(916) 555-1212", "9165551212"},
		{"916.555.1212", "9165551212"},
		{"9165551212", "9165551212"},
		{"1-800-555-1234", "18005551234"},
	}

	for _, test := range tests {
		result := normalizeNumber(test.input)
		if result != test.expected {
			t.Errorf("normalizeNumber(%q) = %q, want %q", test.input, result, test.expected)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	yaml := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3
  idle_timeout: 120

logging:
  level: debug
  format: json

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
    required_init: "ATZ"

rate_limit:
  enabled: true
  max_attempts: 3
  window_seconds: 30
  block_duration: 60
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Port != 2323 {
		t.Errorf("Port = %d, want 2323", cfg.Server.Port)
	}
	if cfg.Server.MaxConnections != 50 {
		t.Errorf("MaxConnections = %d, want 50", cfg.Server.MaxConnections)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Level = %s, want debug", cfg.Logging.Level)
	}
	if len(cfg.Phonebook) != 1 {
		t.Errorf("Phonebook len = %d, want 1", len(cfg.Phonebook))
	}
	if cfg.Phonebook[0].RequiredInit != "ATZ" {
		t.Errorf("RequiredInit = %s, want ATZ", cfg.Phonebook[0].RequiredInit)
	}
	// Backward compat: required_init should be migrated to required_settings.init
	if cfg.Phonebook[0].RequiredSettings.Init != "ATZ" {
		t.Errorf("RequiredSettings.Init = %s, want ATZ", cfg.Phonebook[0].RequiredSettings.Init)
	}
}

func TestLoadConfigWithPassword(t *testing.T) {
	yaml := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Locked BBS"
    password: "swordfish"

  - number: "916-555-1213"
    host: "127.0.0.1"
    port: 23
    name: "Open BBS"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := cfg.Phonebook[0].Password; got != "swordfish" {
		t.Errorf("Phonebook[0].Password = %q, want %q", got, "swordfish")
	}
	if got := cfg.Phonebook[1].Password; got != "" {
		t.Errorf("Phonebook[1].Password = %q, want empty", got)
	}
}

func TestLoadConfigWithRequiredSettings(t *testing.T) {
	yaml := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
    required_settings:
      init: "ATZ"
      baud: 9600
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Phonebook[0].RequiredSettings.Init != "ATZ" {
		t.Errorf("RequiredSettings.Init = %s, want ATZ", cfg.Phonebook[0].RequiredSettings.Init)
	}
	if cfg.Phonebook[0].RequiredSettings.Baud != 9600 {
		t.Errorf("RequiredSettings.Baud = %d, want 9600", cfg.Phonebook[0].RequiredSettings.Baud)
	}
}

func TestLoadConfigInvalidBaud(t *testing.T) {
	yaml := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
    required_settings:
      baud: 12345
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Fatal("expected error for invalid baud rate")
	}
}

func TestErrorCorrectionCompressionDefaults(t *testing.T) {
	yaml := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Default BBS"
  - number: "415-555-0100"
    host: "example.com"
    port: 23
    name: "Raw BBS"
    required_settings:
      error_correction: false
      compression: false
  - number: "800-555-1234"
    host: "example.com"
    port: 23
    name: "Explicit BBS"
    required_settings:
      error_correction: true
      compression: true
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Entry 0: no settings specified → defaults to true
	ec0 := cfg.Phonebook[0].RequiredSettings.ErrorCorrection
	comp0 := cfg.Phonebook[0].RequiredSettings.Compression
	if ec0 == nil || *ec0 != true {
		t.Errorf("entry 0 ErrorCorrection = %v, want true", ec0)
	}
	if comp0 == nil || *comp0 != true {
		t.Errorf("entry 0 Compression = %v, want true", comp0)
	}

	// Entry 1: explicitly false
	ec1 := cfg.Phonebook[1].RequiredSettings.ErrorCorrection
	comp1 := cfg.Phonebook[1].RequiredSettings.Compression
	if ec1 == nil || *ec1 != false {
		t.Errorf("entry 1 ErrorCorrection = %v, want false", ec1)
	}
	if comp1 == nil || *comp1 != false {
		t.Errorf("entry 1 Compression = %v, want false", comp1)
	}

	// Entry 2: explicitly true
	ec2 := cfg.Phonebook[2].RequiredSettings.ErrorCorrection
	comp2 := cfg.Phonebook[2].RequiredSettings.Compression
	if ec2 == nil || *ec2 != true {
		t.Errorf("entry 2 ErrorCorrection = %v, want true", ec2)
	}
	if comp2 == nil || *comp2 != true {
		t.Errorf("entry 2 Compression = %v, want true", comp2)
	}
}

func TestLookupNumber(t *testing.T) {
	cfg := &Config{
		Phonebook: []PhonebookEntry{
			{Number: "916-555-1212", Host: "127.0.0.1", Port: 23, Name: "Test"},
			{Number: "415-555-0100", Host: "example.com", Port: 23, Name: "Example"},
		},
	}

	// Test exact match
	entry := cfg.LookupNumber("916-555-1212")
	if entry == nil {
		t.Fatal("LookupNumber returned nil for valid number")
	}
	if entry.Host != "127.0.0.1" {
		t.Errorf("Host = %s, want 127.0.0.1", entry.Host)
	}

	// Test normalized match
	entry = cfg.LookupNumber("9165551212")
	if entry == nil {
		t.Fatal("LookupNumber returned nil for normalized number")
	}

	// Test non-existent
	entry = cfg.LookupNumber("000-000-0000")
	if entry != nil {
		t.Error("LookupNumber should return nil for unknown number")
	}
}

func TestLoadConfigWithAllowedNetworks(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"

dialer:
  allowed_networks:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Dialer.AllowedNetworks) != 3 {
		t.Fatalf("AllowedNetworks len = %d, want 3", len(cfg.Dialer.AllowedNetworks))
	}

	parsed := cfg.Dialer.ParsedNetworks()
	if len(parsed) != 3 {
		t.Fatalf("ParsedNetworks() len = %d, want 3", len(parsed))
	}

	// Verify that each parsed CIDR matches the expected network
	expectedCIDRs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	for i, expected := range expectedCIDRs {
		if parsed[i].String() != expected {
			t.Errorf("ParsedNetworks()[%d] = %s, want %s", i, parsed[i].String(), expected)
		}
	}
}

func TestLoadConfigInvalidCIDR(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"

dialer:
  allowed_networks:
    - "not-a-cidr"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
	if !strings.Contains(err.Error(), "invalid CIDR") {
		t.Errorf("error should mention 'invalid CIDR', got: %v", err)
	}
}

func TestLoadConfigEmptyAllowedNetworks(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	parsed := cfg.Dialer.ParsedNetworks()
	if len(parsed) != 0 {
		t.Errorf("ParsedNetworks() len = %d, want 0 when no allowed_networks configured", len(parsed))
	}
}

func TestValidateInvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"port zero", 0},
		{"port too high", 70000},
		{"negative port", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yamlData := fmt.Sprintf(`
server:
  port: %d
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
`, tt.port)
			tmpfile, err := os.CreateTemp("", "config*.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tmpfile.Name())

			if _, err := tmpfile.WriteString(yamlData); err != nil {
				t.Fatal(err)
			}
			tmpfile.Close()

			_, err = Load(tmpfile.Name())
			if err == nil {
				t.Fatalf("expected error for port %d", tt.port)
			}
			if !strings.Contains(err.Error(), "port") {
				t.Errorf("error should mention 'port', got: %v", err)
			}
		})
	}
}

func TestValidateInvalidMaxConnections(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 0
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Fatal("expected error for max_connections 0")
	}
	if !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("error should mention 'max_connections', got: %v", err)
	}
}

func TestValidateEmptyPhonebookNumber(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: ""
    host: "127.0.0.1"
    port: 23
    name: "Test BBS"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Fatal("expected error for empty phonebook number")
	}
	if !strings.Contains(err.Error(), "number is required") {
		t.Errorf("error should mention 'number is required', got: %v", err)
	}
}

func TestValidateEmptyPhonebookHost(t *testing.T) {
	yamlData := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    host: ""
    port: 23
    name: "Test BBS"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString(yamlData); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Fatal("expected error for empty phonebook host")
	}
	if !strings.Contains(err.Error(), "host is required") {
		t.Errorf("error should mention 'host is required', got: %v", err)
	}
}

// A busy entry never places a call, so host and port carry nothing — requiring
// them would force an operator to invent an unreachable host for a number that
// is only ever engaged.
func TestLoadConfigBusyEntryNeedsNoHostOrPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telix.yaml")
	content := `
server:
  port: 2323
  max_connections: 50
  max_per_ip: 3

phonebook:
  - number: "916-555-1212"
    name: "Engaged BBS"
    busy: true
  - number: "916-555-1213"
    host: "bbs.example.com"
    port: 23
    name: "Test BBS"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	engaged := cfg.LookupNumber("916-555-1212")
	if engaged == nil {
		t.Fatal("busy entry missing from the phonebook")
	}
	if !engaged.Busy {
		t.Error("busy: true did not survive the load")
	}
	if open := cfg.LookupNumber("916-555-1213"); open == nil || open.Busy {
		t.Errorf("an entry without busy: must not be marked busy, got %+v", open)
	}
}

// The metrics port default is load-bearing outside Go: prometheus.yml scrapes
// telix:9101, so a change here silently breaks scraping rather than failing a
// build.
func TestMetricsConfigDefaults(t *testing.T) {
	tests := []struct {
		name     string
		cfg      MetricsConfig
		wantPort int
		wantAddr string
	}{
		{
			name:     "unset port falls back to 9101 and binds all interfaces",
			cfg:      MetricsConfig{Enabled: true},
			wantPort: 9101,
			wantAddr: ":9101",
		},
		{
			name:     "explicit port and bind are used verbatim",
			cfg:      MetricsConfig{Enabled: true, Port: 9999, Bind: "127.0.0.1"},
			wantPort: 9999,
			wantAddr: "127.0.0.1:9999",
		},
		{
			name:     "a zero port is treated as unset rather than as port zero",
			cfg:      MetricsConfig{Enabled: true, Port: 0, Bind: "0.0.0.0"},
			wantPort: 9101,
			wantAddr: "0.0.0.0:9101",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.GetPort(); got != tt.wantPort {
				t.Errorf("GetPort() = %d, want %d", got, tt.wantPort)
			}
			if got := tt.cfg.Addr(); got != tt.wantAddr {
				t.Errorf("Addr() = %q, want %q", got, tt.wantAddr)
			}
		})
	}
}

func TestLoadConfigParsesMetricsSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telix.yaml")
	content := `
server:
  port: 2323
  max_connections: 100
  max_per_ip: 3
metrics:
  enabled: true
  port: 9101
  bind: "0.0.0.0"
phonebook:
  - number: "555-1234"
    host: "bbs.example.com"
    port: 23
    name: "Test BBS"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.Metrics.Enabled {
		t.Error("Metrics.Enabled = false, want true")
	}
	if got := cfg.Metrics.Addr(); got != "0.0.0.0:9101" {
		t.Errorf("Metrics.Addr() = %q, want \"0.0.0.0:9101\"", got)
	}
}

// A config with no metrics section at all must load and leave metrics off,
// so an existing deployment's telix.yaml keeps working untouched.
func TestLoadConfigWithoutMetricsSectionDisablesMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telix.yaml")
	content := `
server:
  port: 2323
  max_connections: 100
  max_per_ip: 3
phonebook:
  - number: "555-1234"
    host: "bbs.example.com"
    port: 23
    name: "Test BBS"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Error("Metrics.Enabled = true for a config with no metrics section, want false")
	}
}
