package resolver

import (
	"strings"
	"testing"

	"github.com/miekg/dns"

	"dns-server/src/config"
)

func TestAdditionalRecordTypes(t *testing.T) {
	rs := &config.RecordSet{
		MX:    []string{"10 mail.example.com."},
		NS:    []string{"ns1.example.com."},
		SRV:   []string{"10 5 443 service.example.com."},
		CAA:   []string{`0 issue "letsencrypt.org"`},
		PTR:   []string{"host.example.com."},
		SOA:   []string{"ns1.example.com. hostmaster.example.com. 2026091601 3600 900 1209600 300"},
		Other: []string{"SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"},
	}
	r := New(&config.Config{
		Server: config.ServerConfig{DefaultTTL: 300},
		Zones: map[string]*config.ZoneConfig{
			"example.com": {Countries: map[string]*config.RecordSet{"default": rs}},
		},
	})
	for _, tc := range []struct {
		name  string
		qtype uint16
		want  string
	}{
		{"MX", dns.TypeMX, "10 mail.example.com."},
		{"NS", dns.TypeNS, "ns1.example.com."},
		{"SRV", dns.TypeSRV, "10 5 443 service.example.com."},
		{"CAA", dns.TypeCAA, `0 issue "letsencrypt.org"`},
		{"PTR", dns.TypePTR, "host.example.com."},
		{"SOA", dns.TypeSOA, "2026091601"},
		{"SSHFP", dns.TypeSSHFP, "0123456789ABCDEF0123456789ABCDEF01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := r.Resolve("example.com", tc.qtype, "")
			if answer == nil || len(answer.Records) != 1 {
				t.Fatalf("records = %v, want one", answer)
			}
			rr := answer.Records[0]
			if rr.Header().Rrtype != tc.qtype || rr.Header().Ttl != 300 {
				t.Fatalf("wrong type or TTL: %s", rr)
			}
			if got := rr.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("RR %q does not contain %q", got, tc.want)
			}
		})
	}
	if got := r.Resolve("example.com", dns.TypeTLSA, ""); got == nil || len(got.Records) != 0 {
		t.Fatalf("unconfigured type should have no answer: %+v", got)
	}

	zone := r.GetZone("example.com")
	if zone == nil || len(zone.Countries["default"].MX) != 1 || len(zone.Countries["default"].Other) != 1 {
		t.Fatalf("API view dropped new records: %+v", zone)
	}
	dump := r.DumpZones()["example.com"].Countries["default"]
	if len(dump.NS) != 1 || len(dump.Other) != 1 {
		t.Fatalf("config dump dropped new records: %+v", dump)
	}
}

func TestValidateRecordSet(t *testing.T) {
	if err := ValidateRecordSet(RecordSet{MX: []string{"not-a-priority mail.example.com."}}); err == nil {
		t.Fatal("invalid MX record accepted")
	}
	if err := ValidateRecordSet(RecordSet{Other: []string{"NOTATYPE data"}}); err == nil {
		t.Fatal("invalid generic record accepted")
	}
}
