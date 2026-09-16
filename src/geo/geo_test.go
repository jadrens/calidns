package geo

import (
	"testing"
	"time"
)

func TestPurgeExpiredMemoryEntries(t *testing.T) {
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	l := &Lookuper{
		cache: map[uint32]*cacheEntry{
			1: {CountryCode: "JP", ExpiresAt: now.Add(time.Minute)},
			2: {CountryCode: "US", ExpiresAt: now.Add(-time.Minute)},
			3: {CountryCode: "DE", ExpiresAt: now},
			4: nil,
		},
	}

	l.purgeExpiredMemoryEntries(now)

	if len(l.cache) != 1 {
		t.Fatalf("expected one unexpired cache entry, got %d", len(l.cache))
	}
	if _, ok := l.cache[1]; !ok {
		t.Fatal("unexpired cache entry was removed")
	}
}
