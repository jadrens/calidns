package handler

import (
	"net"
	"testing"

	"github.com/miekg/dns"

	"dns-server/src/config"
	"dns-server/src/resolver"
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
