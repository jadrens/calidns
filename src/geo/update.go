package geo

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const maxGeoIPDownloadSize = 128 << 20

// nextGeoIPUpdateDelay uses the actual file modification time, not the
// process start time. A missing file is due immediately.
func nextGeoIPUpdateDelay(path string, interval time.Duration, now time.Time) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	delay := info.ModTime().Add(interval).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func waitForGeoIPUpdate(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *Lookuper) updateLoop(ctx context.Context) {
	for {
		if !waitForGeoIPUpdate(ctx, nextGeoIPUpdateDelay(l.datPath, l.updateInterval, time.Now())) {
			return
		}
		if err := l.updateDat(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[geo] geoip.dat update failed: %v", err)
			retry := l.updateInterval
			if retry > time.Hour {
				retry = time.Hour
			}
			if retry < time.Minute {
				retry = time.Minute
			}
			if !waitForGeoIPUpdate(ctx, retry) {
				return
			}
		} else {
			log.Printf("[geo] geoip.dat updated: %s", l.datPath)
		}
	}
}

// updateDat validates a downloaded file and builds its index before replacing
// the existing data. An unsuccessful download never changes the active index.
func (l *Lookuper) updateDat(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.updateURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxGeoIPDownloadSize {
		return fmt.Errorf("download exceeds %d bytes", maxGeoIPDownloadSize)
	}

	f, err := os.CreateTemp(filepath.Dir(l.datPath), ".geoip-download-*")
	if err != nil {
		return fmt.Errorf("creating download file: %w", err)
	}
	defer os.Remove(f.Name())
	mode := os.FileMode(0644)
	if current, err := os.Stat(l.datPath); err == nil {
		mode = current.Mode().Perm()
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxGeoIPDownloadSize+1))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxGeoIPDownloadSize {
		return fmt.Errorf("download exceeds %d bytes", maxGeoIPDownloadSize)
	}
	if n == 0 {
		return fmt.Errorf("download is empty")
	}
	newIndex, err := loadDatIndex(f.Name(), l.mmap)
	if err != nil {
		return fmt.Errorf("downloaded geoip.dat is invalid: %w", err)
	}
	now := time.Now()
	if err := os.Chtimes(f.Name(), now, now); err != nil {
		_ = newIndex.Close()
		return err
	}
	if err := os.Rename(f.Name(), l.datPath); err != nil {
		_ = newIndex.Close()
		return fmt.Errorf("replacing geoip.dat: %w", err)
	}
	l.localMu.Lock()
	oldIndex := l.local
	l.local = newIndex
	l.localMu.Unlock()
	if oldIndex != nil {
		if err := oldIndex.Close(); err != nil {
			log.Printf("[geo] closing old geoip.dat index: %v", err)
		}
	}
	return nil
}
