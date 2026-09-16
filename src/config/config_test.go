package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateDefaultEmbeddedTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := GenerateDefault(path); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, []byte(defaultConfigYAML)) {
		t.Fatal("generated config differs from embedded template")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
	if cfg.Database.Type != "sqlite" || cfg.Database.SQLitePath != "queries.db" {
		t.Fatalf("unexpected default database: %+v", cfg.Database)
	}
	if err := GenerateDefault(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("generating over existing config: got %v, want os.ErrExist", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, generated) {
		t.Fatalf("existing config changed: %v", err)
	}
}

func TestEnableGeoIPMmap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"default", "server: {}\n", false},
		{"enabled", "server:\n  enable_geoip_mmap: true\n", true},
		{"disabled", "server:\n  enable_geoip_mmap: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.EnableGeoIPMmap != tc.want {
				t.Fatalf("enable_geoip_mmap = %v, want %v", cfg.Server.EnableGeoIPMmap, tc.want)
			}
		})
	}
}

func TestGeoIPUpdateConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		name         string
		yaml         string
		wantInterval string
		wantError    bool
	}{
		{"default interval", "server:\n  geoip_update_url: https://example.com/geoip.dat\n", "24h", false},
		{"custom interval", "server:\n  geoip_update_url: https://example.com/geoip.dat\n  geoip_update_interval: 12h\n", "12h", false},
		{"invalid URL", "server:\n  geoip_update_url: file:///tmp/geoip.dat\n", "", true},
		{"invalid interval", "server:\n  geoip_update_url: https://example.com/geoip.dat\n  geoip_update_interval: yesterday\n", "", true},
		{"negative interval", "server:\n  geoip_update_url: https://example.com/geoip.dat\n  geoip_update_interval: -1h\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err != nil) != tc.wantError {
				t.Fatalf("Load error = %v, wantError=%v", err, tc.wantError)
			}
			if err == nil && cfg.Server.GeoIPUpdateInterval != tc.wantInterval {
				t.Fatalf("interval = %q, want %q", cfg.Server.GeoIPUpdateInterval, tc.wantInterval)
			}
		})
	}
}

func TestDatabaseConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		name, yaml, wantType, wantPath string
	}{
		{"legacy postgres", "database:\n  host: localhost\n", "", ""},
		{"sqlite default path", "database:\n  type: sqlite\n", "sqlite", "queries.db"},
		{"sqlite custom path", "database:\n  type: sqlite\n  sqlite_path: data/queries.db\n", "sqlite", "data/queries.db"},
		{"other type", "database:\n  type: postgresql\n", "postgresql", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Database.Type != tc.wantType || cfg.Database.SQLitePath != tc.wantPath {
				t.Fatalf("database = %+v", cfg.Database)
			}
		})
	}
}
