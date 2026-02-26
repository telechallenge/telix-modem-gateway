package config

import (
	"os"
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
