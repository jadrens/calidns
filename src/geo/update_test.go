package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNextGeoIPUpdateDelayUsesMtime(t *testing.T) {
	path := testDatFile(t)
	now := time.Now().Truncate(time.Second)
	mtime := now.Add(-6 * time.Hour)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if got := nextGeoIPUpdateDelay(path, 24*time.Hour, now); got != 18*time.Hour {
		t.Fatalf("delay = %s, want 18h", got)
	}
	if got := nextGeoIPUpdateDelay(path, 5*time.Hour, now); got != 0 {
		t.Fatalf("overdue delay = %s, want 0", got)
	}
	if got := nextGeoIPUpdateDelay(filepath.Join(t.TempDir(), "missing.dat"), time.Hour, now); got != 0 {
		t.Fatalf("missing file delay = %s, want 0", got)
	}
}

func TestGeoIPUpdateReplacesFileAndLiveIndex(t *testing.T) {
	for _, mmap := range []bool{false, true} {
		name := "heap"
		if mmap {
			name = "mmap"
		}
		t.Run(name, func(t *testing.T) {
			path := testDatFile(t)
			newData := datGroup("CA", "1.2.0.0/16", "2001:db8::/32")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(newData)
			}))
			defer server.Close()
			l, err := NewLookuper(filepath.Join(t.TempDir(), "cache.db"), time.Hour, nil, path, mmap, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			l.updateURL = server.URL
			if got := l.lookupLocal("1.2.4.5"); got != "US" {
				t.Fatalf("initial country = %q", got)
			}
			var readers sync.WaitGroup
			for i := 0; i < 4; i++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for j := 0; j < 100; j++ {
						_ = l.lookupLocal("1.2.4.5")
					}
				}()
			}
			if err := l.updateDat(context.Background()); err != nil {
				t.Fatal(err)
			}
			readers.Wait()
			if got := l.lookupLocal("1.2.4.5"); got != "CA" {
				t.Fatalf("updated country = %q", got)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != string(newData) {
				t.Fatalf("on-disk geoip.dat was not replaced: err=%v", err)
			}
			info, err := os.Stat(path)
			if err != nil || time.Since(info.ModTime()) > time.Minute {
				t.Fatalf("mtime was not refreshed: info=%v err=%v", info, err)
			}
		})
	}
}

func TestGeoIPUpdateRejectsInvalidDownload(t *testing.T) {
	path := testDatFile(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a geoip.dat file"))
	}))
	defer server.Close()
	l, err := NewLookuper(filepath.Join(t.TempDir(), "cache.db"), time.Hour, nil, path, true, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.updateURL = server.URL
	if err := l.updateDat(context.Background()); err == nil {
		t.Fatal("invalid download accepted")
	}
	if got := l.lookupLocal("1.2.4.5"); got != "US" {
		t.Fatalf("old index changed: %q", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("old file changed after failed update: %v", err)
	}
}

func TestGeoIPUpdateLoopRunsWhenMtimeExpired(t *testing.T) {
	path := testDatFile(t)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(datGroup("CA", "1.2.0.0/16"))
		select {
		case called <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	l, err := NewLookuper(filepath.Join(t.TempDir(), "cache.db"), time.Hour, nil, path, false, server.URL, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("overdue geoip.dat was not downloaded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for l.lookupLocal("1.2.4.5") != "CA" {
		if time.Now().After(deadline) {
			t.Fatal("new index was not activated")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGeoIPUpdateLoopRecoversMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geoip.dat")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(datGroup("CA", "1.2.0.0/16"))
	}))
	defer server.Close()
	l, err := NewLookuper(filepath.Join(t.TempDir(), "cache.db"), time.Hour, nil, path, true, server.URL, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	deadline := time.Now().Add(5 * time.Second)
	for l.lookupLocal("1.2.4.5") != "CA" {
		if time.Now().After(deadline) {
			t.Fatal("missing geoip.dat was not downloaded and activated")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("downloaded file not installed: %v", err)
	}
}
