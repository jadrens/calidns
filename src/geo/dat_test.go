package geo

import (
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

func protoVarint(field int, value uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(field<<3))
	out := append([]byte(nil), buf[:n]...)
	n = binary.PutUvarint(buf[:], value)
	return append(out, buf[:n]...)
}

func protoBytes(field int, value []byte) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(field<<3|2))
	out := append([]byte(nil), buf[:n]...)
	n = binary.PutUvarint(buf[:], uint64(len(value)))
	out = append(out, buf[:n]...)
	return append(out, value...)
}

func datGroup(code string, cidrs ...string) []byte {
	group := protoBytes(1, []byte(code))
	for _, cidr := range cidrs {
		prefix := netip.MustParsePrefix(cidr)
		ip := prefix.Addr().AsSlice()
		entry := protoBytes(1, ip)
		entry = append(entry, protoVarint(2, uint64(prefix.Bits()))...)
		group = append(group, protoBytes(2, entry)...)
	}
	return protoBytes(1, group)
}

func testDatFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "geoip.dat")
	data := append(datGroup("US", "1.2.0.0/16", "2001:db8::/32"), datGroup("JP", "1.2.3.0/24", "2001:db8:1::/48")...)
	data = append(data, datGroup("CLOUDFLARE", "1.2.3.0/25", "2001:db8:1::/64")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDatIndexLongestPrefix(t *testing.T) {
	path := testDatFile(t)
	for _, mode := range []struct {
		name string
		mmap bool
	}{{"heap", false}, {"mmap", true}} {
		t.Run(mode.name, func(t *testing.T) {
			d, err := loadDatIndex(path, mode.mmap)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if (d.mapped != nil) != mode.mmap {
				t.Fatalf("wrong storage mode: mmap=%v", d.mapped != nil)
			}
			for _, tc := range []struct{ ip, want string }{
				{"1.2.3.4", "JP"},
				{"1.2.4.5", "US"},
				{"2001:db8:1::1", "JP"},
				{"2001:db8:2::1", "US"},
				{"2.2.2.2", ""},
				{"invalid", ""},
			} {
				if got := d.lookup(tc.ip); got != tc.want {
					t.Errorf("lookup(%q) = %q, want %q", tc.ip, got, tc.want)
				}
			}
			leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".geoip-index-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary mmap index was not removed: %v, %v", leftovers, err)
			}
		})
	}
}

func TestSQLiteCachePrecedesDat(t *testing.T) {
	for _, mmap := range []bool{false, true} {
		name := "heap"
		if mmap {
			name = "mmap"
		}
		t.Run(name, func(t *testing.T) {
			l, err := NewLookuper(filepath.Join(t.TempDir(), "cache.db"), 7*24*time.Hour, "none", testDatFile(t), mmap, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			if _, err := l.db.Exec(`INSERT INTO geo_cache (subnet24, country_code, city, asn, as_name, expires_at) VALUES (?, ?, ?, ?, ?, ?)`, uint32(1<<16|2<<8|3), "FR", "Paris", "123", "Example", expires); err != nil {
				t.Fatal(err)
			}
			cc, city, asn, asName, cached, err := l.Lookup("1.2.3.4")
			if err != nil || cc != "FR" || city != "Paris" || asn != "123" || asName != "Example" || !cached {
				t.Fatalf("SQLite precedence: got (%q, %q, %q, %q, %v, %v)", cc, city, asn, asName, cached, err)
			}
			cc, city, asn, asName, cached, err = l.Lookup("1.2.4.5")
			if err != nil || cc != "US" || city != "" || asn != "" || asName != "" || cached {
				t.Fatalf("dat fallback: got (%q, %q, %q, %q, %v, %v)", cc, city, asn, asName, cached, err)
			}
			var count int
			if err := l.db.QueryRow("SELECT COUNT(*) FROM geo_cache").Scan(&count); err != nil || count != 1 {
				t.Fatalf("dat result should not populate SQLite: count=%d, err=%v", count, err)
			}
		})
	}
}

func TestDatRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.dat")
	if err := os.WriteFile(path, []byte{0x0a, 0x10, 0x01}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDatIndex(path, false); err == nil {
		t.Fatal("expected malformed file to be rejected")
	}
}

func TestBundledDat(t *testing.T) {
	path := filepath.Join("..", "..", "local", "geoip.dat")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("geoip.dat not present")
	}
	d, err := loadDatIndex(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.codes) == 0 || d.lookup("1.1.1.1") == "" {
		t.Fatal("bundled geoip.dat has no usable country data")
	}
	var v4Count, v6Count int
	for _, bucket := range d.v4 {
		v4Count += len(bucket)
	}
	for _, bucket := range d.v6 {
		v6Count += len(bucket)
	}
	t.Logf("country prefixes: IPv4=%d IPv6=%d; index records=%d bytes", v4Count, v6Count,
		uintptr(v4Count)*unsafe.Sizeof(datV4{})+uintptr(v6Count)*unsafe.Sizeof(datV6{}))
}

func TestBundledDatMmap(t *testing.T) {
	path := filepath.Join("..", "..", "local", "geoip.dat")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("geoip.dat not present")
	}
	d, err := loadDatIndex(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.mapped == nil || len(d.v4[24]) != 0 || d.lookup("1.1.1.1") == "" {
		t.Fatal("bundled geoip.dat mmap index is not usable")
	}
	t.Logf("mapped compact index: %d bytes", len(d.mapped))
}

func BenchmarkBundledDatLookup(b *testing.B) {
	for _, mode := range []struct {
		name string
		mmap bool
	}{{"heap", false}, {"mmap", true}} {
		b.Run(mode.name, func(b *testing.B) {
			d, err := loadDatIndex(filepath.Join("..", "..", "local", "geoip.dat"), mode.mmap)
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			ips := [...]string{"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = d.lookup(ips[i%len(ips)])
			}
		})
	}
}

func BenchmarkSQLiteMissDatFallback(b *testing.B) {
	l, err := NewLookuper(filepath.Join(b.TempDir(), "cache.db"), 7*24*time.Hour, "none", filepath.Join("..", "..", "local", "geoip.dat"), false, "", 0)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, _, err := l.Lookup("1.1.1.1"); err != nil {
			b.Fatal(err)
		}
	}
}
