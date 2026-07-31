package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/net/websocket"
)

const maxClients = 1000

// LogEntry 与 ingest 的 LogEntry 结构一致
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Service   string `json:"service,omitempty"`
	Node      string `json:"node,omitempty"`
	Message   string `json:"message"`
}

// Alert 告警消息
type Alert struct {
	ID        string `json:"id"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
	Node      string `json:"node,omitempty"`
	Service   string `json:"service,omitempty"`
}

// Hub 管理所有活跃的 WebSocket 连接
type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
	history []Alert
	maxHist int
}

// RuleEngine 滑动窗口统计日志，触发告警
type RuleEngine struct {
	mu          sync.Mutex
	entries     []timedEntry
	alertCounts map[string]int // cooldown per rule key to avoid flood
}

type timedEntry struct {
	ts      time.Time
	entry   LogEntry
}

type ruleAlert struct {
	id       string
	key      string  // dedup key
	level    string
	title    string
	message  string
	service  string
	node     string
	cooldown time.Duration
}

var (
	hub         = &Hub{clients: make(map[*websocket.Conn]bool), maxHist: 100}
	engine      = &RuleEngine{alertCounts: make(map[string]int)}
	port        = envOr("ALERTER_PORT", "8004")
	alertIDGen  = 0
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ---- Hub 方法 ----

func (h *Hub) add(ws *websocket.Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxClients {
		return websocket.ErrBadStatus
	}
	h.clients[ws] = true
	for _, a := range h.history {
		data, _ := json.Marshal(a)
		websocket.Message.Send(ws, string(data))
	}
	log.Printf("alerter: client connected (%d total)", len(h.clients))
	return nil
}

func (h *Hub) remove(ws *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, ws)
	count := len(h.clients)
	h.mu.Unlock()
	ws.Close()
	log.Printf("alerter: client disconnected (%d remaining)", count)
}

func (h *Hub) broadcast(alert Alert) {
	h.mu.Lock()
	h.history = append(h.history, alert)
	if len(h.history) > h.maxHist {
		h.history = h.history[1:]
	}
	data, _ := json.Marshal(alert)
	msg := string(data)
	clients := make([]*websocket.Conn, 0, len(h.clients))
	for ws := range h.clients {
		clients = append(clients, ws)
	}
	h.mu.Unlock()

	for _, ws := range clients {
		if err := websocket.Message.Send(ws, msg); err != nil {
			h.mu.Lock()
			delete(h.clients, ws)
			h.mu.Unlock()
			ws.Close()
		}
	}
	log.Printf("alerter: broadcast [%s] %s", alert.Level, alert.Title)
}

func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ---- 规则引擎 ----

func (re *RuleEngine) feed(e LogEntry) []ruleAlert {
	re.mu.Lock()
	defer re.mu.Unlock()

	now := time.Now()
	re.entries = append(re.entries, timedEntry{ts: now, entry: e})

	// 清理超过 2 分钟的旧日志
	cutoff := now.Add(-2 * time.Minute)
	valid := re.entries[:0]
	for _, te := range re.entries {
		if te.ts.After(cutoff) {
			valid = append(valid, te)
		}
	}
	re.entries = valid

	// 统计 1 分钟窗口内各级别日志数量
	oneMinAgo := now.Add(-1 * time.Minute)
	var err1m, crit1m int
	var svcErrs = make(map[string]int) // service errors in 2 min

	for _, te := range re.entries {
		lvl := te.entry.Level
		if lvl == "ERROR" {
			svcErrs[te.entry.Service]++
			if te.ts.After(oneMinAgo) {
				err1m++
			}
		}
		if lvl == "CRIT" && te.ts.After(oneMinAgo) {
			crit1m++
		}
	}

	var alerts []ruleAlert

	// 规则1：CRIT 级别立即告警
	if crit1m >= 1 {
		alerts = append(alerts, ruleAlert{
			id:     fmt.Sprintf("crit-%d", alertIDGen),
			key:    "crit-global",
			level:  "crit",
			title:  fmt.Sprintf("CRITICAL: %d 条 CRIT 级别日志", crit1m),
			message: fmt.Sprintf("最近 1 分钟内出现 %d 条 CRIT 日志: %s", crit1m, e.Message),
			service: e.Service,
			node:    e.Node,
			cooldown: 30 * time.Second,
		})
	}

	// 规则2：1 分钟内 ERROR 超过 5 条触发
	if err1m > 5 {
		alerts = append(alerts, ruleAlert{
			id:     fmt.Sprintf("err-burst-%d", alertIDGen),
			key:    "err-burst-global",
			level:  "error",
			title:  fmt.Sprintf("ERROR 突发: 1 分钟内 %d 条错误", err1m),
			message: fmt.Sprintf("错误日志频率异常，请检查服务状态。最新错误: %s", e.Message),
			service: e.Service,
			node:    e.Node,
			cooldown: 60 * time.Second,
		})
	}

	// 规则3：2 分钟内同一服务 ERROR 超过 10 条触发
	for svc, count := range svcErrs {
		if count > 10 {
			key := "svc-err-" + svc
			alerts = append(alerts, ruleAlert{
				id:     fmt.Sprintf("svc-err-%d", alertIDGen),
				key:    key,
				level:  "warn",
				title:  fmt.Sprintf("服务 %s 持续报错: %d 条/2分钟", svc, count),
				message: fmt.Sprintf("服务 %s 在 2 分钟内出现 %d 条 ERROR，可能存在持续故障", svc, count),
				service: svc,
				node:    e.Node,
				cooldown: 120 * time.Second,
			})
		}
	}

	return alerts
}

func (re *RuleEngine) shouldAlert(key string, cooldown time.Duration) bool {
	re.mu.Lock()
	defer re.mu.Unlock()
	last, ok := re.alertCounts[key]
	if ok && time.Now().Unix() < int64(last)+int64(cooldown.Seconds()) {
		return false
	}
	re.alertCounts[key] = int(time.Now().Unix())
	return true
}

func generateID() string {
	alertIDGen++
	return fmt.Sprintf("alert-%d-%d", time.Now().Unix(), alertIDGen)
}

// ---- RabbitMQ 消费 ----

func consumeRabbitMQ() {
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

	for {
		conn, err := amqp.Dial(url)
		if err != nil {
			log.Printf("alerter: RabbitMQ connect failed: %v (retry in 5s)", err)
			time.Sleep(5 * time.Second)
			continue
		}

		ch, err := conn.Channel()
		if err != nil {
			log.Printf("alerter: RabbitMQ channel error: %v", err)
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		_, err = ch.QueueDeclare("logs", true, false, false, false, nil)
		if err != nil {
			log.Printf("alerter: RabbitMQ queue declare error: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		msgs, err := ch.Consume("logs", "alerter", true, false, false, false, nil)
		if err != nil {
			log.Printf("alerter: RabbitMQ consume error: %v", err)
			ch.Close()
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("alerter: RabbitMQ connected, consuming from 'logs'")

		closeCh := conn.NotifyClose(make(chan *amqp.Error))

		consumeLoop:
		for {
			select {
			case err := <-closeCh:
				log.Printf("alerter: RabbitMQ connection lost: %v", err)
				ch.Close()
				conn.Close()
				break consumeLoop
			case msg, ok := <-msgs:
				if !ok {
					break consumeLoop
				}
				var entry LogEntry
				if err := json.Unmarshal(msg.Body, &entry); err != nil {
					continue
				}

				alerts := engine.feed(entry)
				for _, ra := range alerts {
					if !engine.shouldAlert(ra.key, ra.cooldown) {
						continue
					}
					hub.broadcast(Alert{
						ID:        generateID(),
						Level:     ra.level,
						Title:     ra.title,
						Message:   ra.message,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Node:      ra.node,
						Service:   ra.service,
					})
				}
			}
		}

		log.Println("alerter: reconnecting RabbitMQ in 5s...")
		time.Sleep(5 * time.Second)
	}
}

// ---- HTTP 接口 ----

func handleWS(ws *websocket.Conn) {
	if err := hub.add(ws); err != nil {
		ws.Close()
		return
	}
	ws.SetDeadline(time.Now().Add(60 * time.Second))
	var msg string
	for {
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
		ws.SetDeadline(time.Now().Add(60 * time.Second))
	}
	hub.remove(ws)
}

func handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var alert Alert
	if err := json.NewDecoder(r.Body).Decode(&alert); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hub.broadcast(alert)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "broadcast", "clients": hub.count(),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok", "clients": hub.count(), "history": len(hub.history),
	})
}

func handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	hub.mu.RLock()
	alerts := make([]Alert, len(hub.history))
	copy(alerts, hub.history)
	hub.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(alerts)
}

func main() {
	http.Handle("/ws", websocket.Handler(handleWS))
	http.HandleFunc("/push", handlePush)
	http.HandleFunc("/alerts", handleGetAlerts)
	http.HandleFunc("/health", handleHealth)

	srv := &http.Server{
		Addr:         ":" + port,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("alerter: starting on %s", srv.Addr)
		log.Printf("  ws://localhost%s/ws    WebSocket endpoint", srv.Addr)
		log.Printf("  POST /push             push alerts manually")
		log.Printf("  GET  /health           health check")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// 启动 RabbitMQ 消费者
	go consumeRabbitMQ()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("alerter: shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	log.Println("alerter: stopped")
}
