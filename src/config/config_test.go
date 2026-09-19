package config

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if bytes.Contains(generated, []byte("listen: []")) {
		t.Fatal("generated config has no listener addresses")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
	if cfg.Database.Type != "sqlite" || cfg.Database.SQLitePath != "queries.db" {
		t.Fatalf("unexpected default database: %+v", cfg.Database)
	}
	if len(cfg.Server.Listen) == 0 {
		t.Fatal("generated config has no listeners")
	}
	if err := GenerateDefault(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("generating over existing config: got %v, want os.ErrExist", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, generated) {
		t.Fatalf("existing config changed: %v", err)
	}
}

func TestListenAddressesExcludesLoopbackAndDownInterfaces(t *testing.T) {
	interfaces := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Name: "down", Flags: 0},
		{Name: "eth0", Flags: net.FlagUp},
	}
	addrs := map[string][]net.Addr{
		"lo":   {&net.IPNet{IP: net.ParseIP("127.0.0.53")}},
		"down": {&net.IPNet{IP: net.ParseIP("192.0.2.10")}},
		"eth0": {
			&net.IPNet{IP: net.ParseIP("192.0.2.42")},
			&net.IPNet{IP: net.ParseIP("127.0.0.2")},
			&net.IPNet{IP: net.ParseIP("2001:db8::42")},
			&net.IPNet{IP: net.ParseIP("fe80::42")},
		},
	}
	got, err := listenAddresses(interfaces, func(iface net.Interface) ([]net.Addr, error) {
		return addrs[iface.Name], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.0.2.42:53", "[2001:db8::42]:53", "[fe80::42%eth0]:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %v, want %v", got, want)
	}
}

func TestListenAddressesRequiresNonLoopbackIP(t *testing.T) {
	_, err := listenAddresses([]net.Interface{{Name: "lo", Flags: net.FlagUp | net.FlagLoopback}}, func(net.Interface) ([]net.Addr, error) {
		t.Fatal("loopback interface should not be inspected")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected an error when no non-loopback address exists")
	}
}

func TestLoadListenerList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen:\n    - '192.0.2.42:53'\n    - '[2001:db8::42]:53'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Server.Listen, ",") != "192.0.2.42:53,[2001:db8::42]:53" {
		t.Fatalf("listeners = %v", cfg.Server.Listen)
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

func TestLoadDoesNotImportLegacyZones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `
server:
  listen: ["127.0.0.1:1053"]
zones:
  example.com:
    mode: simple
    default:
      a: ["192.0.2.10"]
  '^api\.example\.com\.?$':
    mode: golang
    default:
      a: ["192.0.2.20"]
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Zones) != 0 {
		t.Fatalf("legacy YAML zones were imported: %+v", cfg.Zones)
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
