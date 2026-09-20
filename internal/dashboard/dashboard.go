// Package dashboard serves the production dashboard bundled into the CaliDNS
// binary. The dashboard must be built before compiling the server.
package dashboard

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing/fstest"
	"time"
)

const (
	maxDashboardZipSize      = 64 << 20
	maxDashboardUnpackedSize = 128 << 20
	maxDashboardFiles        = 10_000
)

//go:embed dist
var content embed.FS

// Handler serves a dashboard build and refreshes remote ZIP sources.
type Handler struct {
	mu       sync.RWMutex
	current  http.Handler
	source   string
	interval time.Duration
	done     chan struct{}
	close    sync.Once
}

// NewHandler loads source and returns a handler for the /dashboard URL prefix.
// "default" selects the embedded build; any other source is an HTTP(S) ZIP
// whose root (or single wrapper directory) contains index.html and its assets.
func NewHandler(source string, interval time.Duration) (*Handler, error) {
	var dist fs.FS
	var err error
	if source == "default" {
		dist, err = fs.Sub(content, "dist")
	} else {
		dist, err = downloadDashboard(source)
	}
	if err != nil {
		return nil, err
	}
	h := &Handler{
		current:  serveDist(dist),
		source:   source,
		interval: interval,
		done:     make(chan struct{}),
	}
	if source != "default" && interval > 0 {
		go h.updateLoop()
	}
	return h, nil
}

// ServeHTTP serves the most recently validated dashboard build.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	current := h.current
	h.mu.RUnlock()
	current.ServeHTTP(w, r)
}

// Close stops remote dashboard updates.
func (h *Handler) Close() error {
	h.close.Do(func() { close(h.done) })
	return nil
}

func (h *Handler) updateLoop() {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dist, err := downloadDashboard(h.source)
			if err != nil {
				log.Printf("[dashboard] update failed: %v", err)
				continue
			}
			h.mu.Lock()
			h.current = serveDist(dist)
			h.mu.Unlock()
			log.Printf("[dashboard] updated from remote ZIP")
		case <-h.done:
			return
		}
	}
}

func serveDist(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/dashboard/")
		if name != "" {
			if info, err := fs.Stat(dist, name); err == nil && !info.IsDir() {
				http.StripPrefix("/dashboard/", files).ServeHTTP(w, r)
				return
			}
		}

		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "dashboard is unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	})
}

func downloadDashboard(source string) (fs.FS, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("creating dashboard request: %w", err)
	}
	req.Header.Set("Accept", "application/zip")
	req.Header.Set("User-Agent", "CaliDNS")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading dashboard ZIP: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading dashboard ZIP: unexpected HTTP status %s", resp.Status)
	}
	if resp.ContentLength > maxDashboardZipSize {
		return nil, fmt.Errorf("dashboard ZIP exceeds %d bytes", maxDashboardZipSize)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDashboardZipSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading dashboard ZIP: %w", err)
	}
	if len(data) > maxDashboardZipSize {
		return nil, fmt.Errorf("dashboard ZIP exceeds %d bytes", maxDashboardZipSize)
	}
	return dashboardZipFS(data)
}

func dashboardZipFS(data []byte) (fs.FS, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("opening dashboard ZIP: %w", err)
	}
	if len(zr.File) > maxDashboardFiles {
		return nil, fmt.Errorf("dashboard ZIP contains more than %d entries", maxDashboardFiles)
	}

	prefix := ""
	for _, file := range zr.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "index.html" {
			prefix = ""
			break
		}
		if strings.HasSuffix(name, "/index.html") && (prefix == "" || len(name)-len("index.html") < len(prefix)) {
			prefix = strings.TrimSuffix(name, "index.html")
		}
	}
	if prefix == "" {
		found := false
		for _, file := range zr.File {
			if strings.TrimSuffix(file.Name, "/") == "index.html" {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("dashboard ZIP does not contain index.html")
		}
	}

	result := make(fstest.MapFS)
	var unpacked uint64
	for _, file := range zr.File {
		if !strings.HasPrefix(file.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSuffix(file.Name, "/"), prefix)
		if name == "" || file.FileInfo().IsDir() {
			continue
		}
		if path.Clean(name) != name || !fs.ValidPath(name) || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("dashboard ZIP contains invalid path %q", file.Name)
		}
		if !file.Mode().IsRegular() {
			return nil, fmt.Errorf("dashboard ZIP contains non-regular file %q", file.Name)
		}
		unpacked += file.UncompressedSize64
		if unpacked > maxDashboardUnpackedSize {
			return nil, fmt.Errorf("dashboard ZIP expands beyond %d bytes", maxDashboardUnpackedSize)
		}
		r, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("opening %q in dashboard ZIP: %w", file.Name, err)
		}
		contents, readErr := io.ReadAll(io.LimitReader(r, int64(file.UncompressedSize64)+1))
		closeErr := r.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading %q in dashboard ZIP: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing %q in dashboard ZIP: %w", file.Name, closeErr)
		}
		if uint64(len(contents)) != file.UncompressedSize64 {
			return nil, fmt.Errorf("dashboard ZIP entry %q has an invalid size", file.Name)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("dashboard ZIP contains duplicate path %q", name)
		}
		result[name] = &fstest.MapFile{Data: contents, Mode: 0444, ModTime: file.Modified}
	}
	if _, ok := result["index.html"]; !ok {
		return nil, fmt.Errorf("dashboard ZIP does not have index.html at its root")
	}
	return result, nil
}
