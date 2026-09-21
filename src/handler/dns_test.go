package handler

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"

	"calidns/src/config"
	"calidns/src/geo"
	"calidns/src/recorder"
	"calidns/src/resolver"
)

type captureWriter struct{ msg *dns.Msg }

func (w *captureWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (w *captureWriter) RemoteAddr() net.Addr      { return &net.UDPAddr{IP: net.ParseIP("127.0.0.1")} }
func (w *captureWriter) WriteMsg(m *dns.Msg) error { w.msg = m; return nil }
func (w *captureWriter) Write([]byte) (int, error) { return 0, nil }
func (w *captureWriter) Close() error              { return nil }
func (w *captureWriter) TsigStatus() error         { return nil }
func (w *captureWriter) TsigTimersOnly(bool)       {}
func (w *captureWriter) Hijack()                   {}

func TestServeDNSAdditionalTypes(t *testing.T) {
	fastOpen := true
	cfg := &config.Config{
		Server: config.ServerConfig{DefaultTTL: 300},
		Zones: map[string]*config.ZoneConfig{
			"example.com": {
				FastOpen: &fastOpen,
				Countries: map[string]*config.RecordSet{
					"default": {MX: []string{"10 mail.example.com."}, NS: []string{"ns1.example.com."}},
				},
			},
		},
	}
	h := New(cfg, resolver.New(cfg), nil, nil)
	for _, qtype := range []uint16{dns.TypeMX, dns.TypeNS} {
		t.Run(dns.TypeToString[qtype], func(t *testing.T) {
			query := new(dns.Msg)
			query.SetQuestion("example.com.", qtype)
			w := new(captureWriter)
			h.ServeDNS(w, query)
			if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 1 || w.msg.Answer[0].Header().Rrtype != qtype {
				t.Fatalf("unexpected DNS response: %+v", w.msg)
			}
		})
	}
}

func TestServeDNSProtocolSemantics(t *testing.T) {
	fastOpen := true
	cfg := &config.Config{
		Server: config.ServerConfig{DefaultTTL: 300, DefaultResponse: "refuse"},
		Zones: map[string]*config.ZoneConfig{
			"alias.example": {FastOpen: &fastOpen, Countries: map[string]*config.RecordSet{"default": {CNAME: []string{"target.example."}}}},
			"soa.example":   {FastOpen: &fastOpen, Countries: map[string]*config.RecordSet{"default": {}}},
		},
	}
	h := New(cfg, resolver.New(cfg), nil, nil)

	query := new(dns.Msg)
	query.SetQuestion("alias.example.", dns.TypeA)
	query.SetEdns0(1232, true)
	w := new(captureWriter)
	h.ServeDNS(w, query)
	if !w.msg.Authoritative || len(w.msg.Answer) != 1 || w.msg.Answer[0].Header().Rrtype != dns.TypeCNAME || w.msg.IsEdns0() == nil {
		t.Fatalf("bad CNAME/EDNS response: %+v", w.msg)
	}

	query.SetQuestion("soa.example.", dns.TypeSOA)
	w = new(captureWriter)
	h.ServeDNS(w, query)
	if len(w.msg.Answer) != 1 || w.msg.Answer[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("SOA was not returned in answer: %+v", w.msg)
	}

	query.SetQuestion("outside.example.", dns.TypeA)
	w = new(captureWriter)
	h.ServeDNS(w, query)
	if w.msg.Authoritative {
		t.Fatalf("unmatched response incorrectly has AA: %+v", w.msg)
	}

	query.SetQuestion("alias.example.", dns.TypeA)
	query.Question[0].Qclass = dns.ClassCHAOS
	w = new(captureWriter)
	h.ServeDNS(w, query)
	if w.msg.Rcode != dns.RcodeNotImplemented || len(w.msg.Answer) != 0 {
		t.Fatalf("non-IN query was answered: %+v", w.msg)
	}
}

type delayedProvider struct{ delay time.Duration }

func (p delayedProvider) Lookup(context.Context, string) (geo.ProviderResult, error) {
	time.Sleep(p.delay)
	cc := "JP"
	return geo.ProviderResult{CountryCode: &cc}, nil
}

func TestRecordingOnlyGeoLookupDoesNotBlockDNS(t *testing.T) {
	lookuper, err := geo.NewLookuper(filepath.Join(t.TempDir(), "geo.db"), time.Hour, delayedProvider{delay: 200 * time.Millisecond}, "", false, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lookuper.Close()
	rec, err := recorder.New("sqlite", filepath.Join(t.TempDir(), "queries.db"), lookuper)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	record, fastOpen := true, true
	cfg := &config.Config{Server: config.ServerConfig{DefaultTTL: 300}, Zones: map[string]*config.ZoneConfig{
		"example.com": {Record: &record, FastOpen: &fastOpen, Countries: map[string]*config.RecordSet{"default": {A: []string{"192.0.2.1"}}}},
	}}
	h := New(cfg, resolver.New(cfg), lookuper, rec)
	query := new(dns.Msg)
	query.SetQuestion("example.com.", dns.TypeA)
	started := time.Now()
	h.ServeDNS(new(captureWriter), query)
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("recording delayed DNS response by %s", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := rec.GetStats()
		if err == nil && stats.TotalQueries == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("record was not persisted asynchronously")
}
