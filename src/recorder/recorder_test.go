package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRecorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queries.db")
	r, err := New("sqlite", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	first := &Entry{
		Domain: "Example.COM", QueryType: "A", ClientIP: "192.0.2.17",
		CountryCode: "JP", City: "Tokyo", GeoCached: true, ASN: "64500", ASName: "Example AS",
		EDNSSubnet: "198.51.100.0/24", EDNSCountryCode: "US", EDNSCity: "Test City",
		EDNSASN: "64501", EDNSASName: "EDNS AS", NSID: "test-nsid",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	second := &Entry{
		Domain: "ipv6.example", QueryType: "AAAA", ClientIP: "2001:db8::1",
		CountryCode: "US", City: "Test", Timestamp: time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC),
	}
	r.insertOne(first)
	r.insertOne(second)
	if got := r.GetStats(); got.TotalQueries != 2 || got.CacheHited != 1 || got.Dropped != 0 {
		t.Fatalf("stats = %+v", got)
	}

	got, err := r.QueryHistory(0, "example.com", "", "192.0.2.0/24", "JP", "2026-01-01T00:00:00Z", "2026-01-02T23:59:59Z", 10, 0)
	if err != nil || got.Total != 1 || len(got.Items) != 1 {
		t.Fatalf("filtered history = %+v, %v", got, err)
	}
	if got.Items[0].EDNSSubnet != first.EDNSSubnet || got.Items[0].EDNSASN != first.EDNSASN || got.Items[0].ASN != first.ASN {
		t.Fatalf("joined history = %+v", got.Items[0])
	}
	firstID := got.Items[0].ID
	if firstID <= 0 {
		t.Fatalf("invalid id %d", firstID)
	}

	edns, err := r.QueryEDNS(firstID, first.EDNSSubnet, "US", first.NSID, "2026-01-01T00:00:00Z", "2026-01-04T00:00:00Z", 10, 0)
	if err != nil || edns.Total != 1 || len(edns.Items) != 1 || edns.Items[0].EDNSASN != first.EDNSASN {
		t.Fatalf("edns = %+v, %v", edns, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sqlite file: %v", err)
	}
	r, err = New("sqlite", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.GetStats(); got.TotalQueries != 2 {
		t.Fatalf("persisted stats = %+v", got)
	}

	deleted, err := r.DeleteEDNS(firstID, "", "", "", "2026-01-01T00:00:00Z", "2026-01-04T00:00:00Z")
	if err != nil || deleted != 1 {
		t.Fatalf("delete edns = %d, %v", deleted, err)
	}
	deleted, err = r.DeleteQueries(0, "", "", "2001:db8::/32", "", "", "")
	if err != nil || deleted != 1 {
		t.Fatalf("delete IPv6 subnet = %d, %v", deleted, err)
	}
	deleted, err = r.DeleteBefore("2026-01-03T00:00:00Z")
	if err != nil || deleted != 1 {
		t.Fatalf("delete before = %d, %v", deleted, err)
	}
	if got := r.GetStats(); got.TotalQueries != 0 {
		t.Fatalf("final stats = %+v", got)
	}
}

func TestSQLiteDeleteQueriesRemovesEDNS(t *testing.T) {
	r, err := New("sqlite", filepath.Join(t.TempDir(), "with edns.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.insertOne(&Entry{
		Domain: "example.com", QueryType: "A", ClientIP: "203.0.113.42",
		CountryCode: "JP", City: "Tokyo", EDNSSubnet: "198.51.100.0/24",
		EDNSCountryCode: "US", EDNSCity: "Test", NSID: "test",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	deleted, err := r.DeleteQueries(0, "example.com", "", "203.0.113.0/24", "", "2026-01-02T12:00:00+09:00", "2026-01-03T00:00:00Z")
	if err != nil || deleted != 1 {
		t.Fatalf("delete queries = %d, %v", deleted, err)
	}
	var remaining int
	if err := r.db.QueryRow("SELECT COUNT(*) FROM edns").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining edns = %d, %v", remaining, err)
	}
}
