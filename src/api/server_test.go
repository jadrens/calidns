package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"

	"dns-server/src/config"
	"dns-server/src/resolver"
)

func TestZoneAPIAdditionalRecordsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Server: config.ServerConfig{Listen: []string{"127.0.0.1:1053"}, DefaultTTL: 300},
		Zones:  map[string]*config.ZoneConfig{},
	}
	res := resolver.New(cfg)
	server := NewServer(res, nil, nil, nil, config.CORSConfig{}, cfg, path)
	body := []byte(`{
		"pattern":"example.com",
		"mode":"simple",
		"countries":{"default":{
			"mx":["10 mail.example.com."],
			"ns":["ns1.example.com."],
			"srv":["10 5 443 service.example.com."],
			"caa":["0 issue \"letsencrypt.org\""],
			"ptr":["host.example.com."],
			"soa":["ns1.example.com. hostmaster.example.com. 2026091601 3600 900 1209600 300"],
			"other":["SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"]
		}}
	}`)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/zones", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/zones?pattern=example.com", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	var zone resolver.ZoneEntry
	if err := json.Unmarshal(w.Body.Bytes(), &zone); err != nil {
		t.Fatal(err)
	}
	if zone.Mode != "simple" || len(zone.Countries["default"].MX) != 1 || len(zone.Countries["default"].Other) != 1 {
		t.Fatalf("API response dropped new records: %+v", zone)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Zones["example.com"].Mode != "simple" {
		t.Fatalf("persisted mode = %q", reloaded.Zones["example.com"].Mode)
	}
	r := resolver.New(reloaded)
	for _, qtype := range []uint16{dns.TypeMX, dns.TypeNS, dns.TypeSRV, dns.TypeCAA, dns.TypePTR, dns.TypeSOA, dns.TypeSSHFP} {
		answer := r.Resolve("example.com", qtype, "")
		if answer == nil || len(answer.Records) != 1 || answer.Records[0].Header().Rrtype != qtype {
			t.Fatalf("reloaded qtype=%d answer=%+v", qtype, answer)
		}
	}
}

func TestZoneAPIRejectsInvalidMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Server: config.ServerConfig{DefaultTTL: 300}, Zones: map[string]*config.ZoneConfig{}}
	res := resolver.New(cfg)
	server := NewServer(res, nil, nil, nil, config.CORSConfig{}, cfg, path)
	body := []byte(`{"pattern":"example.com","mode":"wildcard","countries":{"default":{"a":["192.0.2.10"]}}}`)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/zones", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if res.GetZone("example.com") != nil {
		t.Fatal("invalid mode changed the resolver")
	}
}

func TestZoneAPIRejectsInvalidRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Server: config.ServerConfig{DefaultTTL: 300}, Zones: map[string]*config.ZoneConfig{}}
	res := resolver.New(cfg)
	server := NewServer(res, nil, nil, nil, config.CORSConfig{}, cfg, path)
	body := []byte(`{"pattern":"example.com","countries":{"default":{"mx":["invalid mail.example.com."]}}}`)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/zones", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if res.GetZone("example.com") != nil {
		t.Fatal("invalid record changed the resolver")
	}
}
