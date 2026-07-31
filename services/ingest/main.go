package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LogEntry 一条日志
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Service   string `json:"service,omitempty"`
	Node      string `json:"node,omitempty"`
	Message   string `json:"message"`
}

// IngestResponse POST /ingest 的返回值
type IngestResponse struct {
	RequestID string `json:"request_id"`
	Ingested  int    `json:"ingested"`
	QueueSize int    `json:"queue_size"`
}

// Stats 服务运行统计
type Stats struct {
	TotalIngested int       `json:"total_ingested"`
	QueueSize     int       `json:"queue_size"`
	Uptime        string    `json:"uptime"`
	StartTime     time.Time `json:"start_time"`
}

var (
	listenPort = envOr("INGEST_PORT", "8001")
	logBuffer  = NewRingBuffer(10000)
	totalCount int
	startTime  = time.Now()
	mu         sync.RWMutex
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// RingBuffer 线程安全的环形缓冲区，同时把新日志推给所有 SSE 客户端。\n// 在 RingBuffer 里广播，避免了并发下"按全局队列长度切片"的不安全问题
type RingBuffer struct {
	mu          sync.RWMutex
	data        []LogEntry
	max         int
	head        int
	size        int
	subscribers map[chan []LogEntry]struct{}
}

func NewRingBuffer(max int) *RingBuffer {
	return &RingBuffer{
		data:        make([]LogEntry, max),
		max:         max,
		subscribers: make(map[chan []LogEntry]struct{}),
	}
}

func (rb *RingBuffer) Push(entries []LogEntry) {
	rb.mu.Lock()
	for _, e := range entries {
		rb.data[rb.head] = e
		rb.head = (rb.head + 1) % rb.max
		if rb.size < rb.max {
			rb.size++
		}
	}
	subs := make([]chan []LogEntry, 0, len(rb.subscribers))
	for ch := range rb.subscribers {
		subs = append(subs, ch)
	}
	rb.mu.Unlock()

	// 非阻塞分发：慢消费者直接丢弃，不拖慢采集
	for _, ch := range subs {
		select {
		case ch <- entries:
		default:
		}
	}
}

func (rb *RingBuffer) Snapshot(lastN int) []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	if rb.size == 0 {
		return nil
	}
	n := lastN
	if n > rb.size {
		n = rb.size
	}
	out := make([]LogEntry, n)
	start := (rb.head - rb.size + rb.max) % rb.max
	for i := 0; i < n; i++ {
		idx := (start + rb.size - n + i) % rb.max
		out[i] = rb.data[idx]
	}
	return out
}

func (rb *RingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.size
}

// Subscribe 返回一个带缓冲的 channel，接收新日志批次。调用方必须调 Unsubscribe 防止泄漏
func (rb *RingBuffer) Subscribe() chan []LogEntry {
	ch := make(chan []LogEntry, 16)
	rb.mu.Lock()
	rb.subscribers[ch] = struct{}{}
	rb.mu.Unlock()
	return ch
}

func (rb *RingBuffer) Unsubscribe(ch chan []LogEntry) {
	rb.mu.Lock()
	delete(rb.subscribers, ch)
	rb.mu.Unlock()
	close(ch)
}

func genRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- RabbitMQ 发布 ----
var (
	rmqConn    *amqp.Connection
	rmqCh      *amqp.Channel
	rmqMu      sync.Mutex
	rmqEnabled bool
)

func initRabbitMQ() {
	host := os.Getenv("RABBITMQ_HOST")
	if host == "" {
		host = "rabbitmq.loghawk"
	}
	port := os.Getenv("RABBITMQ_AMQP_PORT")
	if port == "" {
		port = "5672"
	}
	user := os.Getenv("RABBITMQ_USER")
	if user == "" {
		user = "guest"
	}
	pass := os.Getenv("RABBITMQ_PASS")
	if pass == "" {
		pass = "guest"
	}

	url := fmt.Sprintf("amqp://%s:%s@%s:%s/", user, pass, host, port)

	var err error
	for i := 0; i < 10; i++ {
		rmqConn, err = amqp.Dial(url)
		if err == nil {
			break
		}
		log.Printf("[INGEST] RabbitMQ connect attempt %d/10: %v", i+1, err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		log.Printf("[INGEST] RabbitMQ unavailable, continuing without queue publish: %v", err)
		return
	}

	rmqCh, err = rmqConn.Channel()
	if err != nil {
		log.Printf("[INGEST] RabbitMQ channel error: %v", err)
		return
	}

	_, err = rmqCh.QueueDeclare("logs", true, false, false, false, nil)
	if err != nil {
		log.Printf("[INGEST] RabbitMQ queue declare error: %v", err)
		return
	}

	rmqEnabled = true
	log.Printf("[INGEST] RabbitMQ connected, publishing to queue 'logs'")

	// 监听连接断开，自动重连
	go func() {
		closeErr := <-rmqConn.NotifyClose(make(chan *amqp.Error))
		log.Printf("[INGEST] RabbitMQ connection lost: %v", closeErr)
		rmqEnabled = false
	}()
}

func publishToQueue(entries []LogEntry) {
	if !rmqEnabled || rmqCh == nil {
		return
	}
	rmqMu.Lock()
	defer rmqMu.Unlock()
	for _, e := range entries {
		body, _ := json.Marshal(e)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := rmqCh.PublishWithContext(ctx, "", "logs", false, false,
			amqp.Publishing{ContentType: "application/json", Body: body})
		cancel()
		if err != nil {
			log.Printf("[INGEST] RabbitMQ publish error: %v", err)
			rmqEnabled = false
			return
		}
	}
}


func handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entries []LogEntry
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid body: %v"}`, err), http.StatusBadRequest)
		return
	}
	if len(entries) == 0 {
		http.Error(w, `{"error":"empty payload"}`, http.StatusBadRequest)
		return
	}

	logBuffer.Push(entries)
	go publishToQueue(entries)
	mu.Lock()
	totalCount += len(entries)
	mu.Unlock()
	atomic.AddInt64(&metricIngestReqs, 1)
	atomic.AddInt64(&metricEntriesTotal, int64(len(entries)))

	reqID := genRequestID()
	log.Printf("[INGEST] request_id=%s ingested=%d queue_size=%d", reqID, len(entries), logBuffer.Len())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(IngestResponse{
		RequestID: reqID,
		Ingested:  len(entries),
		QueueSize: logBuffer.Len(),
	})
}


func handleGetLogs(w http.ResponseWriter, r *http.Request) {
	entries := logBuffer.Snapshot(500)
	if entries == nil {
		entries = []LogEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

/stream -- SSE
func handleLogStream(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&metricSSEClients, 1)
	defer atomic.AddInt64(&metricSSEClients, -1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher := w.(http.Flusher)

	// 订阅 RingBuffer，每个客户端独立接收新日志
	// notifications instead of slicing based on global queue size, which is
	// unsafe when multiple clients consume concurrently and the buffer wraps.
	sub := logBuffer.Subscribe()
	defer logBuffer.Unsubscribe(sub)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case entries := <-sub:
				if len(entries) > 0 {
					data, _ := json.Marshal(entries)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			default:
				// no new entries since last tick
			}
		}
	}
}

// GET /health
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","queue_size":%d}`, logBuffer.Len())
}

// GET /stats
func handleStats(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	s := Stats{
		TotalIngested: totalCount,
		QueueSize:     logBuffer.Len(),
		Uptime:        time.Since(startTime).Round(time.Second).String(),
		StartTime:     startTime,
	}
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

// ===== PROMETHEUS METRICS =====
var (
	metricIngestReqs   int64
	metricEntriesTotal int64
	metricSSEClients   int64
	metricsStart       = time.Now()
)

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	queueSize := logBuffer.Len()
	uptime := time.Since(metricsStart).Seconds()
	reqs := atomic.LoadInt64(&metricIngestReqs)
	entries := atomic.LoadInt64(&metricEntriesTotal)
	sse := atomic.LoadInt64(&metricSSEClients)
	fmt.Fprintf(w, `# HELP loghawk_ingest_requests_total Total ingest requests
# TYPE loghawk_ingest_requests_total counter
loghawk_ingest_requests_total %d
# HELP loghawk_ingest_entries_total Total log entries ingested
# TYPE loghawk_ingest_entries_total counter
loghawk_ingest_entries_total %d
# HELP loghawk_ingest_queue_size Current ring buffer queue size
# TYPE loghawk_ingest_queue_size gauge
loghawk_ingest_queue_size %d
# HELP loghawk_ingest_sse_clients Active SSE stream clients
# TYPE loghawk_ingest_sse_clients gauge
loghawk_ingest_sse_clients %d
# HELP loghawk_ingest_uptime_seconds Service uptime in seconds
# TYPE loghawk_ingest_uptime_seconds gauge
loghawk_ingest_uptime_seconds %.0f
`, reqs, entries, queueSize, sse, uptime)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") || strings.Contains(origin, ".loghawk") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	// Connect to RabbitMQ (non-fatal, service continues without it)
	go initRabbitMQ()

	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", handleIngest)
	mux.HandleFunc("/logs", handleGetLogs)
	mux.HandleFunc("/logs/stream", handleLogStream)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/metrics", handleMetrics)

	srv := &http.Server{
		Addr:         ":" + listenPort,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("[INGEST] starting on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[INGEST] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	log.Println("[INGEST] stopped")
}
