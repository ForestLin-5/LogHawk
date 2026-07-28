package main

import (
        "context"
        "os/signal"
        "syscall"
        "encoding/json"
        "log"
        "net/http"
        "os"
        "sync"
        "time"

        "golang.org/x/net/websocket"
)

const maxClients = 1000

// Alert represents a structured alert message
type Alert struct {
        ID        string `json:"id"`
        Level     string `json:"level"`
        Title     string `json:"title"`
        Message   string `json:"message"`
        Timestamp string `json:"timestamp"`
        Node      string `json:"node,omitempty"`
        Service   string `json:"service,omitempty"`
}

// Hub maintains the set of active WebSocket clients
type Hub struct {
        mu      sync.RWMutex
        clients map[*websocket.Conn]bool
        history []Alert
        maxHist int
}

var (
        hub  = &Hub{clients: make(map[*websocket.Conn]bool), maxHist: 100}
        port = envOr("ALERTER_PORT", "8004")
)

func envOr(k, d string) string {
        if v := os.Getenv(k); v != "" {
                return v
        }
        return d
}

func (h *Hub) add(ws *websocket.Conn) error {
        h.mu.Lock()
        defer h.mu.Unlock()
        if len(h.clients) >= maxClients {
                return websocket.ErrBadStatus
        }
        h.clients[ws] = true
        count := len(h.clients)
        // Send history to new client
        for _, a := range h.history {
                data, _ := json.Marshal(a)
                websocket.Message.Send(ws, string(data))
        }
        log.Printf("?? Client connected (%d total)", count)
        return nil
}

func (h *Hub) remove(ws *websocket.Conn) {
        h.mu.Lock()
        delete(h.clients, ws)
        count := len(h.clients)
        h.mu.Unlock()
        ws.Close()
        log.Printf("?? Client disconnected (%d remaining)", count)
}

func (h *Hub) broadcast(alert Alert) {
        h.mu.Lock()
        // Store in history ring
        h.history = append(h.history, alert)
        if len(h.history) > h.maxHist {
                h.history = h.history[1:]
        }

        data, _ := json.Marshal(alert)
        msg := string(data)

        // Snapshot clients while holding lock
        clients := make([]*websocket.Conn, 0, len(h.clients))
        for ws := range h.clients {
                clients = append(clients, ws)
        }
        h.mu.Unlock()

        // Send without holding lock (avoids head-of-line blocking)
        for _, ws := range clients {
                if err := websocket.Message.Send(ws, msg); err != nil {
                        h.mu.Lock()
                        delete(h.clients, ws)
                        h.mu.Unlock()
                        ws.Close()
                }
        }
        log.Printf("?? Broadcast alert to %d clients: [%s] %s", h.count(), alert.Level, alert.Title)
}

func (h *Hub) count() int {
        h.mu.RLock()
        defer h.mu.RUnlock()
        return len(h.clients)
}

func handleWS(ws *websocket.Conn) {
        if err := hub.add(ws); err != nil {
                ws.Close()
                return
        }
        // Keep connection alive with ping/pong
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

func main() {
        http.Handle("/ws", websocket.Handler(handleWS))
        http.HandleFunc("/push", handlePush)
        http.HandleFunc("/health", handleHealth)

        srv := &http.Server{
                Addr:         ":" + port,
                ReadTimeout:  10 * time.Second,
                WriteTimeout: 10 * time.Second,
                IdleTimeout:  120 * time.Second,
        }

        go func() {
                log.Printf("?? LogHawk Alerter starting on %s", srv.Addr)
                log.Printf("   ws://localhost%s/ws       WebSocket endpoint", srv.Addr)
                log.Printf("   POST /push                push alerts")
                log.Printf("   GET  /health              health check")
                if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                        log.Fatal(err)
                }
        }()

        quit := make(chan os.Signal, 1)
        signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
        <-quit
        log.Println("?? Shutting down alerter...")

        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := srv.Shutdown(ctx); err != nil {
                log.Printf("Shutdown error: %v", err)
        }
        log.Println("? Alerter stopped")
}
