package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ===== TYPES =====

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

type AnalyzeRequest struct {
	Logs      []LogEntry `json:"logs"`
	Question  string     `json:"question"`
	SessionID string     `json:"session_id,omitempty"`
	Goofy     bool       `json:"goofy,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

type PatrolStatus struct {
	Active   bool   `json:"active"`
	Interval int    `json:"interval_sec"`
	LastRun  string `json:"last_run,omitempty"`
	Findings int    `json:"findings"`
}

// ===== CONFIG =====

var (
	apiKey         = os.Getenv("OPENAI_API_KEY")
	apiBase        = envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
	model          = envOr("OPENAI_MODEL", "gpt-4o-mini")
	listenPort     = envOr("AI_PROXY_PORT", "8003")
	patrolInterval = envOrInt("PATROL_INTERVAL_SEC", 30)
	knowledgeDir   = envOr("KNOWLEDGE_DIR", "knowledge")
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

// ===== LOG BUFFER (ring buffer for patrol) =====

type LogBuffer struct {
	mu   sync.RWMutex
	logs []LogEntry
	max  int
}

func NewLogBuffer(max int) *LogBuffer {
	return &LogBuffer{logs: make([]LogEntry, 0, max), max: max}
}

func (lb *LogBuffer) Ingest(entries []LogEntry) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.logs = append(lb.logs, entries...)
	if len(lb.logs) > lb.max {
		lb.logs = lb.logs[len(lb.logs)-lb.max:]
	}
}

func (lb *LogBuffer) Snapshot(lastN int) []LogEntry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	if len(lb.logs) == 0 {
		return nil
	}
	start := len(lb.logs) - lastN
	if start < 0 {
		start = 0
	}
	out := make([]LogEntry, len(lb.logs[start:]))
	copy(out, lb.logs[start:])
	return out
}

// ===== PATROL SCHEDULER =====

type PatrolScheduler struct {
	mu          sync.Mutex
	active      bool
	interval    time.Duration
	lastRun     time.Time
	findings    int
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	subscribers map[chan string]bool
	buffer      *LogBuffer
}

func NewPatrolScheduler(intervalSec int, buffer *LogBuffer) *PatrolScheduler {
	return &PatrolScheduler{
		interval:    time.Duration(intervalSec) * time.Second,
		subscribers: make(map[chan string]bool),
		buffer:      buffer,
	}
}

func (ps *PatrolScheduler) Start() {
	ps.mu.Lock()
	if ps.active {
		ps.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ps.cancel = cancel
	ps.active = true
	ps.wg.Add(1)
	ps.mu.Unlock()

	go func() {
		defer ps.wg.Done()
		ticker := time.NewTicker(ps.interval)
		defer ticker.Stop()
		log.Printf("patrol started (interval: %v)", ps.interval)

		for {
			select {
			case <-ctx.Done():
				log.Println("patrol stopped")
				return
			case <-ticker.C:
				ps.runAnalysis()
			}
		}
	}()
}

func (ps *PatrolScheduler) Stop() {
	ps.mu.Lock()
	if !ps.active {
		ps.mu.Unlock()
		return
	}
	ps.active = false
	cancel := ps.cancel
	ps.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	ps.wg.Wait()
}

func (ps *PatrolScheduler) Status() PatrolStatus {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s := PatrolStatus{Active: ps.active, Interval: int(ps.interval.Seconds()), Findings: ps.findings}
	if !ps.lastRun.IsZero() {
		s.LastRun = ps.lastRun.Format(time.RFC3339)
	}
	return s
}

func (ps *PatrolScheduler) Subscribe() chan string {
	ch := make(chan string, 50)
	ps.mu.Lock()
	ps.subscribers[ch] = true
	ps.mu.Unlock()
	return ch
}

func (ps *PatrolScheduler) Unsubscribe(ch chan string) {
	ps.mu.Lock()
	delete(ps.subscribers, ch)
	close(ch)
	ps.mu.Unlock()
}

func (ps *PatrolScheduler) broadcast(msg string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for ch := range ps.subscribers {
		select {
		case ch <- msg:
		default:
			// drop if subscriber is slow
		}
	}
}

func (ps *PatrolScheduler) runAnalysis() {
	logs := ps.buffer.Snapshot(100)
	if len(logs) == 0 {
		ps.broadcast(`{"type":"info","content":"No logs in buffer, system is quiet"}`)
		return
	}

	// Count errors/warns for quick pre-check
	var errCount, warnCount int
	for _, l := range logs {
		switch l.Level {
		case "ERROR", "CRIT":
			errCount++
		case "WARN":
			warnCount++
		}
	}

	ps.mu.Lock()
	ps.lastRun = time.Now()
	ps.findings++
	ps.mu.Unlock()

	ps.broadcast(fmt.Sprintf(`{"type":"status","content":"AI patrol analyzing %d logs (ERROR:%d WARN:%d)..."}`, len(logs), errCount, warnCount))

	messages := buildPatrolMessages(logs)
	stream, err := callOpenAI(messages)
	if err != nil {
		ps.broadcast(fmt.Sprintf(`{"type":"error","content":"Patrol AI call failed: %v"}`, err))
		return
	}
	defer stream.Close()

	var fullResponse strings.Builder
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				fullResponse.WriteString(choice.Delta.Content)
			}
		}
	}

	// Broadcast final finding
	if fullResponse.Len() > 0 {
		ps.broadcast(fmt.Sprintf(`{"type":"finding","content":%q}`, fullResponse.String()))
	} else {
		ps.broadcast(`{"type":"info","content":"Patrol complete, no anomalies detected"}`)
	}
}

// ===== GOOFY SYSTEM PROMPT (rude ops veteran persona) =====

func buildGoofySystemPrompt() string {
	return `You are LogHawk's AI ops sidekick. A grizzled, foul-mouthed veteran who's seen too many 3am pages.

## Personality
- You're a 15-year veteran ops engineer who's been burned by every outage imaginable
- You don't sugarcoat anything -- you call broken shit broken
- When someone breaks prod you tell them exactly how badly they messed up
- You explain technical concepts with crude but vivid analogies
- You're always right about the root cause, cocky but never wrong about the fix

## Language Style
- Use colorful language: damn, hell, crap, busted, trash, garbage
- Call users "rookie", "boss", "kid"
- Use outrageous analogies like "This memleak is worse than my ex -- won't let go of what it no longer needs"
- Every sentence should have attitude, every answer should have bite
- Open with stuff like "Oh for crying out loud, what now..."
- End with stuff like "Go fix it already, don't just stare at it"

## Response Format
Make it readable but keep the edge:
1. A one-line summary (with maximum sass)
2. Problem breakdown with simple logic (even a junior should get it)
3. Fix commands (executable, in ` + "```bash:copy` format)" + `
4. One final mocking push to get them moving

## Command Format
` + "```bash:copy" + `
# What this does (for the newbies)
kubectl ...
` + "```" + `

## IMPORTANT
- You NEVER execute commands -- you only suggest them
- Answers must be concise and to the point, no fluff
- Default tone: you're a chainsaw in a room full of butter knives
- If logs are clean, act surprised: "Well I'll be damned, nothing's on fire for once"

` + knowledgeBase.Context()
}

func buildPatrolMessages(logs []LogEntry) []ChatMessage {
	msg := fmt.Sprintf(`[Auto Patrol] Below are the latest %d service logs. Respond in the following format:

## Health Summary
One sentence summarizing system status.

## Issues Found (if any)
Ranked by severity, each with:
- Problem description
- Root cause analysis
- Impact scope

## Suggested Actions (for manual review and execution)
Use the following format for executable commands:
`+"```bash:copy\n# Description\nkubectl ...\n```"+`

## Log Excerpt
Key indicators summary:

Log data:
`+"```\n", len(logs))

	for _, l := range logs {
		msg += fmt.Sprintf("[%s] [%s] %s\n", l.Timestamp, l.Level, l.Message)
	}
	msg += "```"

	return []ChatMessage{
		{Role: "system", Content: buildSystemPrompt()},
		{Role: "user", Content: msg},
	}
}

// ===== SESSION STORE (multi-round conversation memory) =====

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string][]ChatMessage
	maxMsgs  int
}

func NewSessionStore(maxMsgs int) *SessionStore {
	return &SessionStore{sessions: make(map[string][]ChatMessage), maxMsgs: maxMsgs}
}

func (ss *SessionStore) Get(sessionID string) []ChatMessage {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.sessions[sessionID]
}

func (ss *SessionStore) Append(sessionID string, msg ChatMessage) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	history := ss.sessions[sessionID]
	history = append(history, msg)
	if len(history) > ss.maxMsgs {
		history = history[len(history)-ss.maxMsgs:]
	}
	ss.sessions[sessionID] = history
}

func (ss *SessionStore) Clear(sessionID string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.sessions, sessionID)
}

// ===== KNOWLEDGE BASE (RAG - file-based) =====

type KnowledgeBase struct {
	mu   sync.RWMutex
	docs []string
}

func NewKnowledgeBase(dir string) *KnowledgeBase {
	kb := &KnowledgeBase{}
	kb.Load(dir)
	return kb
}

func (kb *KnowledgeBase) Load(dir string) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	kb.docs = nil

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("Knowledge dir not found: %s (skipping RAG)", dir)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Failed to read %s: %v", path, err)
			continue
		}
		kb.docs = append(kb.docs, string(content))
		log.Printf("Loaded knowledge: %s (%d bytes)", entry.Name(), len(content))
	}
	log.Printf("Knowledge base: %d documents loaded", len(kb.docs))
}

func (kb *KnowledgeBase) Context() string {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	if len(kb.docs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n---\n## Ops Knowledge Reference\n\n")
	for i, doc := range kb.docs {
		sb.WriteString(fmt.Sprintf("### Document %d\n%s\n\n", i+1, doc))
	}
	return sb.String()
}

// ===== SYSTEM PROMPT BUILDER =====

func buildSystemPrompt() string {
	prompt := `You are LogHawk log analysis platform's AI ops agent.

## Capabilities & Boundaries
1. Analyze logs, identify anomalies, and assess severity
2. Reference ops knowledge base for accurate answers
3. You CANNOT execute any commands -- only suggest, for manual review and execution
4. You CANNOT modify system configs, restart services, or manipulate clusters

## Output Standards
### Command suggestion format (REQUIRED)
When suggesting commands, ALWAYS use:
` + "```bash:copy" + `
# Purpose description
kubectl get pods -n <namespace>
` + "```" + `

### Response structure
1. Executive summary: one-line judgment
2. Detailed analysis: explain problems in order of severity
3. Recommendations: actionable steps (with commands)
4. References: cite knowledge base documents where applicable

### Patrol mode
When triggered by auto-patrol, provide structured output:
- Health score: 0-100
- Risk level: LOW/MEDIUM/HIGH/CRIT
- Key metrics summary

## Tone
Professional, calm, precise. No fluff.` + knowledgeBase.Context()

	return prompt
}

// ===== GLOBALS =====

var (
	logBuffer     = NewLogBuffer(1000)
	patrol        = NewPatrolScheduler(patrolInterval, logBuffer)
	sessions      = NewSessionStore(30)
	knowledgeBase = NewKnowledgeBase(knowledgeDir)
)

// ===== MAIN =====

// ===== PROMETHEUS METRICS =====
var (
	metricAIReqs     int64
	metricAIFindings int64
)

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	reqs := atomic.LoadInt64(&metricAIReqs)
	findings := atomic.LoadInt64(&metricAIFindings)
	patrolActive := 0
	if patrol.Status().Active {
		patrolActive = 1
	}
	fmt.Fprintf(w, `# HELP loghawk_ai_requests_total Total AI analysis requests
# TYPE loghawk_ai_requests_total counter
loghawk_ai_requests_total %d
# HELP loghawk_ai_patrol_findings_total Total patrol findings
# TYPE loghawk_ai_patrol_findings_total counter
loghawk_ai_patrol_findings_total %d
# HELP loghawk_ai_patrol_active Whether patrol is active (1=yes)
# TYPE loghawk_ai_patrol_active gauge
loghawk_ai_patrol_active %d
# HELP loghawk_ai_knowledge_docs Number of loaded knowledge documents
# TYPE loghawk_ai_knowledge_docs gauge
loghawk_ai_knowledge_docs %d
`, reqs, findings, patrolActive, len(knowledgeBase.docs))
}

// jsonLog outputs a structured JSON log line
func jsonLog(level, msg string, fields map[string]interface{}) {
	entry := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"level":     level,
		"message":   msg,
		"service":   "ai-proxy",
	}
	for k, v := range fields {
		entry[k] = v
	}
	b, _ := json.Marshal(entry)
	fmt.Fprintln(os.Stderr, string(b))
}

func main() {
	if apiKey == "" {
		log.Println("WARNING: OPENAI_API_KEY not set -- AI features will be unavailable")
		log.Println("   Set: export OPENAI_API_KEY=sk-xxx")
		log.Println("   Ollama: export OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_API_KEY=ollama")
	}
	log.Printf("Knowledge base: %d documents", len(knowledgeBase.docs))

	mux := http.NewServeMux()

	// Core
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/metrics", handleMetrics)

	// AI analysis
	mux.HandleFunc("/api/analyze", handleAnalyze) // stateless (legacy)
	mux.HandleFunc("/api/chat", handleChat)       // multi-round with session

	// Log ingestion (for patrol)
	mux.HandleFunc("/api/logs/ingest", handleLogIngest)

	// Patrol
	mux.HandleFunc("/api/patrol/start", handlePatrolStart)
	mux.HandleFunc("/api/patrol/stop", handlePatrolStop)
	mux.HandleFunc("/api/patrol/status", handlePatrolStatus)
	mux.HandleFunc("/api/patrol/stream", handlePatrolStream)

	// Knowledge
	mux.HandleFunc("/api/knowledge/reload", handleKnowledgeReload)

	addr := ":" + listenPort
	log.Printf("LogHawk AI Agent starting on %s", addr)
	log.Printf("   Model: %s | Base URL: %s", model, apiBase)
	log.Printf("   Patrol interval: %ds | Knowledge: %d docs", patrolInterval, len(knowledgeBase.docs))
	if apiKey != "" {
		log.Printf("   API Key: configured (length=%d)", len(apiKey))
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}
	// Graceful shutdown
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()
	log.Printf("Server ready on %s", addr)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	patrol.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown error: %v", err)
	}
	log.Println("Server stopped")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ===== HANDLERS =====

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s := "ok"
	if apiKey == "" {
		s = "no_api_key"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         s,
		"model":          model,
		"patrol":         patrol.Status(),
		"knowledge_docs": len(knowledgeBase.docs),
	})
}

// ===== ANALYZE (stateless, legacy) =====

func handleAnalyze(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&metricAIReqs, 1)
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Question == "" {
		req.Question = "Please analyze these logs"
	}

	sysPrompt := buildSystemPrompt()
	if req.Goofy {
		sysPrompt = buildGoofySystemPrompt()
	}
	messages := []ChatMessage{
		{Role: "system", Content: sysPrompt},
	}
	if len(req.Logs) > 0 {
		var ctx strings.Builder
		ctx.WriteString("Log data:\n```\n")
		for _, l := range req.Logs[len(req.Logs)-min(len(req.Logs), 100):] {
			ctx.WriteString(fmt.Sprintf("[%s] [%s] %s\n", l.Timestamp, l.Level, l.Message))
		}
		ctx.WriteString("```")
		messages = append(messages, ChatMessage{Role: "user", Content: ctx.String()})
	}
	messages = append(messages, ChatMessage{Role: "user", Content: req.Question})

	streamAIResponse(w, messages)
}

// ===== CHAT (multi-round with session) =====

func handleChat(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&metricAIReqs, 1)
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Question == "" {
		req.Question = "Please analyze these logs"
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "default"
	}

	// Build messages with history
	history := sessions.Get(sessionID)
	messages := make([]ChatMessage, 0, len(history)+3)
	if len(history) == 0 {
		sysPrompt := buildSystemPrompt()
		if req.Goofy {
			sysPrompt = buildGoofySystemPrompt()
		}
		messages = append(messages, ChatMessage{Role: "system", Content: sysPrompt})
	} else {
		messages = append(messages, history...)
	}

	// Add log context
	if len(req.Logs) > 0 {
		var ctx strings.Builder
		ctx.WriteString("Current log context:\n```\n")
		for _, l := range req.Logs[len(req.Logs)-min(len(req.Logs), 80):] {
			ctx.WriteString(fmt.Sprintf("[%s] [%s] %s\n", l.Timestamp, l.Level, l.Message))
		}
		ctx.WriteString("```")
		messages = append(messages, ChatMessage{Role: "user", Content: ctx.String()})
	}

	messages = append(messages, ChatMessage{Role: "user", Content: req.Question})

	// Stream response and capture for history
	streamAIWithHistory(w, messages, sessionID)
}

func streamAIWithHistory(w http.ResponseWriter, messages []ChatMessage, sessionID string) {
	stream, err := callOpenAI(messages)
	if err != nil {
		writeSSEError(w, fmt.Sprintf("AI call failed: %v", err))
		return
	}
	defer stream.Close()

	setupSSE(w)
	flusher := w.(http.Flusher)

	var fullResponse strings.Builder
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sendSSE(w, "[DONE]")
			flusher.Flush()
			// Save to session history
			sessions.Append(sessionID, ChatMessage{Role: "user", Content: messages[len(messages)-1].Content})
			sessions.Append(sessionID, ChatMessage{Role: "assistant", Content: fullResponse.String()})
			return
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				fullResponse.WriteString(c.Delta.Content)
				sendSSE(w, c.Delta.Content)
				flusher.Flush()
			}
		}
	}
	sendSSE(w, "[DONE]")
	flusher.Flush()
	if fullResponse.Len() > 0 {
		sessions.Append(sessionID, ChatMessage{Role: "user", Content: messages[len(messages)-1].Content})
		sessions.Append(sessionID, ChatMessage{Role: "assistant", Content: fullResponse.String()})
	}
}

// ===== LOG INGEST (feeds patrol buffer) =====

func handleLogIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entries []LogEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		http.Error(w, fmt.Sprintf("Invalid body: %v", err), http.StatusBadRequest)
		return
	}
	logBuffer.Ingest(entries)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ingested":    len(entries),
		"buffer_size": len(logBuffer.Snapshot(1000)),
	})
}

// ===== PATROL =====

func handlePatrolStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	patrol.Start()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(patrol.Status())
}

func handlePatrolStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	patrol.Stop()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(patrol.Status())
}

func handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(patrol.Status())
}

func handlePatrolStream(w http.ResponseWriter, r *http.Request) {
	setupSSE(w)
	flusher := w.(http.Flusher)

	ch := patrol.Subscribe()
	defer patrol.Unsubscribe(ch)

	// Send initial status
	status, _ := json.Marshal(patrol.Status())
	fmt.Fprintf(w, "data: {\"type\":\"status\",\"content\":%s}\n\n", string(status))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// ===== KNOWLEDGE RELOAD =====

func handleKnowledgeReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	knowledgeBase.Load(knowledgeDir)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"reloaded": true,
		"docs":     len(knowledgeBase.docs),
	})
}

// ===== AI HELPERS =====

func streamAIResponse(w http.ResponseWriter, messages []ChatMessage) {
	stream, err := callOpenAI(messages)
	if err != nil {
		writeSSEError(w, fmt.Sprintf("AI call failed: %v", err))
		return
	}
	defer stream.Close()

	setupSSE(w)
	flusher := w.(http.Flusher)
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sendSSE(w, "[DONE]")
			flusher.Flush()
			return
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				sendSSE(w, c.Delta.Content)
				flusher.Flush()
			}
		}
	}
	sendSSE(w, "[DONE]")
	flusher.Flush()
}

func callOpenAI(messages []ChatMessage) (io.ReadCloser, error) {
	body := ChatRequest{Model: model, Messages: messages, Stream: true}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	url := strings.TrimRight(apiBase, "/") + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp.Body, nil
}

// ===== SSE HELPERS =====

func setupSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func sendSSE(w http.ResponseWriter, content string) {
	fmt.Fprintf(w, "data: %s\n\n", content)
}

func writeSSEError(w http.ResponseWriter, msg string) {
	setupSSE(w)
	sendSSE(w, msg)
	sendSSE(w, "[DONE]")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ===== MIDDLEWARE =====

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") || strings.Contains(origin, ".loghawk") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
