package dnsdata

import (
	"path/filepath"
	"testing"

	"calidns/src/config"
)

func TestStoreInitializesAndPersistsDNSData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns_data.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	defaults, err := store.LoadDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if defaults.TTL != 300 || defaults.Record || defaults.Response != "refuse" {
		t.Fatalf("initial defaults = %+v", defaults)
	}
	defaults = Defaults{TTL: 600, Record: true, Response: "nxdomain"}
	if err := store.SaveDefaults(defaults); err != nil {
		t.Fatal(err)
	}
	zone := &config.ZoneConfig{
		Mode: "simple",
		Countries: map[string]*config.RecordSet{
			"default": {A: []string{"192.0.2.10"}},
		},
	}
	if err := store.UpsertZone("example.com", zone); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gotDefaults, err := store.LoadDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if gotDefaults != defaults {
		t.Fatalf("defaults = %+v, want %+v", gotDefaults, defaults)
	}
	zones, err := store.LoadZones()
	if err != nil {
		t.Fatal(err)
	}
	if zones["example.com"].Countries["default"].A[0] != "192.0.2.10" {
		t.Fatalf("zones = %+v", zones)
	}
}
