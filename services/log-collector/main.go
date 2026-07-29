package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Service   string `json:"service,omitempty"`
	Node      string `json:"node,omitempty"`
	Message   string `json:"message"`
}

type criLogLine struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

var (
	ingestURL    = envOr("INGEST_URL", "http://ingest.loghawk:8001/ingest")
	nodeName     = envOr("NODE_NAME", "unknown")
	logDir       = envOr("LOG_DIR", "/var/log/containers")
	offsetsFile  = envOr("OFFSETS_FILE", "/var/lib/loghawk/offsets.json")
	batchSize    = envOrInt("BATCH_SIZE", 100)
	flushInt     = envOrDuration("FLUSH_INTERVAL", 5*time.Second)
	maxRetries   = envOrInt("MAX_RETRIES", 3)
	httpClient   = &http.Client{Timeout: 15 * time.Second}
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return fallback
}

func envOrDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// OffsetStore persists per-file byte offsets so restarts resume without duplicates.
type OffsetStore struct {
	mu      sync.RWMutex
	offsets map[string]int64
	path    string
}

// shim os methods for testability
var (
	osReadFile  = os.ReadFile
	osWriteFile = os.WriteFile
	osMkdirAll  = os.MkdirAll
	osStat      = os.Stat
	osOpen      = os.Open
	osReadDir   = os.ReadDir
)

func NewOffsetStore(path string) (*OffsetStore, error) {
	s := &OffsetStore{offsets: make(map[string]int64), path: path}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		log.Printf("offset load warning: %v", err)
	}
	return s, nil
}

func (s *OffsetStore) load() error {
	data, err := osReadFile(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(data, &s.offsets)
}

func (s *OffsetStore) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.offsets, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := osMkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return osWriteFile(s.path, data, 0o600)
}

func (s *OffsetStore) Get(path string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.offsets[path]
}

func (s *OffsetStore) Set(path string, off int64) {
	s.mu.Lock()
	s.offsets[path] = off
	s.mu.Unlock()
}

// Collector reads container logs and ships them to ingest with retries.
type Collector struct {
	store  *OffsetStore
	http   *http.Client
	ingest string
	node   string
}

func NewCollector(store *OffsetStore) *Collector {
	return &Collector{store: store, http: httpClient, ingest: ingestURL, node: nodeName}
}

func (c *Collector) Collect(ctx context.Context) ([]LogEntry, error) {
	entries := make([]LogEntry, 0, batchSize)

	files, err := osReadDir(logDir)
	if err != nil {
		return nil, fmt.Errorf("read log dir: %w", err)
	}

	for _, file := range files {
		select {
		case <-ctx.Done():
			return entries, ctx.Err()
		default:
		}

		if file.IsDir() {
			continue
		}
		name := file.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}

		path := filepath.Join(logDir, name)
		info, err := osStat(path)
		if err != nil {
			log.Printf("stat %s: %v", path, err)
			continue
		}

		size := info.Size()
		offset := c.store.Get(path)
		if size <= offset {
			continue
		}

		f, err := osOpen(path)
		if err != nil {
			log.Printf("open %s: %v", path, err)
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			log.Printf("seek %s: %v", path, err)
			continue
		}

		data, err := io.ReadAll(io.LimitReader(f, 50*1024*1024))
		f.Close()
		if err != nil {
			log.Printf("read %s: %v", path, err)
			continue
		}

		svc := serviceName(name)
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			msg := extractMessage(line)
			entries = append(entries, LogEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Level:     detectLevel(msg),
				Service:   svc,
				Node:      c.node,
				Message:   msg,
			})
			if len(entries) >= batchSize {
				break
			}
		}

		// Only advance offset for bytes we successfully parsed. If we hit the
		// batch limit before EOF we still advance to current file size; the
		// remaining bytes will be collected on the next tick.
		c.store.Set(path, size)
		if len(entries) >= batchSize {
			break
		}
	}
	return entries, nil
}

func serviceName(fileName string) string {
	// CRI log filename: <pod>_<namespace>_<container>-<container-id>.log
	name := strings.TrimSuffix(fileName, ".log")
	// Strip container ID hash (last segment after '-')
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		name = name[:idx]
	}
	// name is now <pod>_<namespace>_<container>
	// Extract container name (last segment after '_')
	if idx := strings.LastIndex(name, "_"); idx > 0 {
		return name[idx+1:]
	}
	return name
}

func extractMessage(raw string) string {
	// Try plain JSON first (legacy or custom format)
	var cri criLogLine
	if err := json.Unmarshal([]byte(raw), &cri); err == nil && cri.Log != "" {
		return strings.TrimSpace(cri.Log)
	}
	// CRI-O / containerd JSON format:
	// YYYY-MM-DDTHH:MM:SS... stderr F {"log":"...","stream":"...","time":"..."}
	if idx := strings.Index(raw, "{"); idx > 0 {
		if err := json.Unmarshal([]byte(raw[idx:]), &cri); err == nil && cri.Log != "" {
			return strings.TrimSpace(cri.Log)
		}
	}
	// CRI plain text format:
	// YYYY-MM-DDTHH:MM:SS... stderr F <actual log content>
	if parts := strings.SplitN(raw, " ", 4); len(parts) >= 4 {
		return strings.TrimSpace(parts[3])
	}
	return raw
}

func detectLevel(msg string) string {
	upper := strings.ToUpper(msg)
	switch {
	case strings.Contains(upper, "CRIT"), strings.Contains(upper, "FATAL"):
		return "CRIT"
	case strings.Contains(upper, "ERROR"):
		return "ERROR"
	case strings.Contains(upper, "WARN"), strings.Contains(upper, "WARNING"):
		return "WARN"
	default:
		return "INFO"
	}
}

func (c *Collector) Send(entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * time.Second
			log.Printf("retrying ingest in %v (attempt %d/%d)", backoff, attempt, maxRetries)
			time.Sleep(backoff)
		}
		resp, err := c.http.Post(c.ingest, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("ingest returned %d: %s", resp.StatusCode, string(respBody))
	}
	return fmt.Errorf("ingest failed after %d retries: %w", maxRetries, lastErr)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","node":"%s","ingest_url":"%s"}`, nodeName, ingestURL)
}

func main() {
	log.Printf("LogHawk Collector starting on node %s", nodeName)
	log.Printf("Watching: %s", logDir)
	log.Printf("Target:   %s", ingestURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := NewOffsetStore(offsetsFile)
	if err != nil {
		log.Fatalf("offset store: %v", err)
	}
	collector := NewCollector(store)

	http.HandleFunc("/health", handleHealth)
	healthSrv := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server error: %v", err)
		}
	}()

	ticker := time.NewTicker(flushInt)
	defer ticker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	flush := func() {
		entries, err := collector.Collect(ctx)
		if err != nil {
			log.Printf("collect error: %v", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		if err := collector.Send(entries); err != nil {
			log.Printf("send error: %v", err)
			return
		}
		if err := store.save(); err != nil {
			log.Printf("offset save error: %v", err)
			return
		}
		log.Printf("sent %d entries from node %s", len(entries), nodeName)
	}

	for {
		select {
		case <-quit:
			log.Println("Shutting down collector...")
			cancel()
			flush()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if err := healthSrv.Shutdown(shutdownCtx); err != nil {
				log.Printf("health shutdown error: %v", err)
			}
			if err := store.save(); err != nil {
				log.Printf("final offset save error: %v", err)
			}
			log.Println("Collector stopped")
			return
		case <-ticker.C:
			flush()
		}
	}
}
