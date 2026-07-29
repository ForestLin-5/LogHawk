# LogHawk API 参考文档

## Ingest Service (端口 8001)

### POST /ingest — 推送日志

批量接收日志条目。

**Request:**
```json
[
  {
    "timestamp": "15:22:10.123",
    "level": "ERROR",
    "service": "api-gateway",
    "node": "k8s-worker1",
    "message": "Connection pool exhausted | active=200 max=200"
  }
]
```

**Response (200):**
```json
{
  "request_id": "a1b2c3d4e5f6g7h8",
  "ingested": 1,
  "queue_size": 42
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `timestamp` | string | 时间戳（HH:MM:SS.mmm） |
| `level` | string | 日志级别：INFO / WARN / ERROR / CRIT |
| `service` | string | 来源服务名（可选） |
| `node` | string | 来源节点（可选） |
| `message` | string | 日志内容 |

**错误码：**
| 状态码 | 说明 |
|--------|------|
| 200 | 摄入成功 |
| 400 | 请求体格式错误或为空 |

---

### GET /logs — 拉取日志

下游服务拉取最近 N 条日志（消费模式）。

**Response (200):**
```json
[
  {
    "timestamp": "15:22:10.123",
    "level": "ERROR",
    "message": "Connection pool exhausted"
  }
]
```

最多返回最近 500 条。

---

### GET /logs/stream — SSE 实时流

Server-Sent Events 端点，每秒推送增量日志。

**Response:** `text/event-stream`
```
data: [{"timestamp":"...","level":"ERROR","message":"..."}]

data: [{"timestamp":"...","level":"WARN","message":"..."}]
```

支持断线重连：重连后自动补齐增量数据。

---

### GET /health — 健康检查

**Response (200):**
```json
{"status":"ok","queue_size":42}
```

---

### GET /stats — 统计信息

**Response (200):**
```json
{
  "total_ingested": 15234,
  "queue_size": 42,
  "uptime": "2h34m15s",
  "start_time": "2026-07-27T12:00:00Z"
}
```

---

## AI Proxy (端口 8003)

### POST /api/analyze — 单次分析

无状态日志分析。

**Request:**
```json
{
  "logs": [
    {"timestamp":"15:22:10.123","level":"ERROR","message":"Connection pool exhausted"}
  ],
  "question": "分析这些日志",
  "goofy": false
}
```

**Response:** `text/event-stream` (SSE 流式)
```
data: 检测到数据库连接池耗尽...

data: 根因分析：连接池配置过小...

data: [DONE]
```

| 字段 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `logs` | array | 否 | 日志条目数组 |
| `question` | string | 否 | 分析问题（默认"请分析以下日志"） |
| `goofy` | bool | 否 | 是否使用暴躁老哥语气（笨鹰模式） |

---

### POST /api/chat — 多轮对话

带会话记忆的分析端点。

**Request:**
```json
{
  "logs": [...],
  "question": "刚才那个问题跟这次的有关联吗？",
  "session_id": "session-abc123",
  "goofy": false
}
```

**Response:** `text/event-stream` (SSE 流式)

| 字段 | 说明 |
|------|------|
| `session_id` | 会话 ID（不传则使用 "default"） |

会话自动保留最近 30 条消息。

---

### POST /api/patrol/start — 启动自主巡检

**Response (200):**
```json
{"active":true,"interval_sec":30,"last_run":"","findings":1}
```

巡检启动后，每 30 秒自动分析日志缓冲区并推送结果到 `/api/patrol/stream`。

---

### POST /api/patrol/stop — 停止巡检

**Response (200):**
```json
{"active":false,"interval_sec":30,"last_run":"2026-07-27T15:30:00Z","findings":12}
```

---

### GET /api/patrol/status — 巡检状态

**Response (200):**
```json
{"active":true,"interval_sec":30,"last_run":"2026-07-27T15:30:00Z","findings":12}
```

---

### GET /api/patrol/stream — 巡检结果流

SSE 端点，实时接收巡检发现。

**Response:** `text/event-stream`
```json
data: {"type":"status","content":"🔍 正在巡检 87 条日志 (ERROR:3 WARN:5)..."}

data: {"type":"finding","content":"## 🟢 健康评估\n系统整体正常..."}
```

| type | 说明 |
|------|------|
| `status` | 巡检进度信息 |
| `finding` | 巡检发现（AI 分析结果） |
| `error` | 巡检错误 |
| `info` | 一般信息 |

---

### POST /api/logs/ingest — 日志摄入（供巡检）

前端的日志流定期推送到此端点，供巡检 Agent 分析。

**Request:**
```json
[
  {"timestamp":"15:22:10.123","level":"ERROR","message":"Connection pool exhausted"}
]
```

**Response (200):**
```json
{"ingested":1,"buffer_size":87}
```

---

### POST /api/knowledge/reload — 热重载知识库

**Response (200):**
```json
{"reloaded":true,"docs":3}
```

---

### GET /health — 健康检查

**Response (200):**
```json
{
  "status":"ok",
  "model":"gpt-4o-mini",
  "patrol":{"active":true,"interval_sec":30,"findings":12},
  "knowledge_docs":3
}
```

---

## Alerter (端口 8004)

Alerter 消费 RabbitMQ 日志队列，经规则引擎匹配后生成告警。

### GET /alerts — 获取历史告警

返回最近 100 条告警记录。

**Response (200):**
```json
[
  {
    "id": "alert-1785255725-1",
    "level": "crit",
    "title": "CRITICAL: 1 条 CRIT 级别日志",
    "message": "最近 1 分钟内出现 1 条 CRIT 日志: 数据库连接池耗尽",
    "timestamp": "2026-07-28T16:23:44Z",
    "service": "payment"
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 唯一标识 |
| `level` | string | crit / error / warn / info |
| `title` | string | 告警标题 |
| `message` | string | 详细描述 |
| `timestamp` | string | UTC 时间 RFC3339 |
| `service` | string | 触发服务 |
| `node` | string | 触发节点 |

---

### POST /push — 手动推送告警

**Request:**
```json
{
  "id": "manual-001",
  "level": "warn",
  "title": "磁盘使用率超过 80%",
  "message": "节点 k8s-worker1 磁盘使用率达 85%",
  "service": "node-monitor",
  "node": "k8s-worker1"
}
```

**Response (202):**
```json
{"status":"broadcast","clients":1}
```

---

### WS /ws — WebSocket 实时推送

连接后自动收到历史告警，新告警实时推送。

```
ws://alerter:8004/ws
```

消息格式同 Alert JSON，每条一个 WebSocket Frame。

---

### GET /health — 健康检查

**Response (200):**
```json
{"status":"ok","clients":1,"history":8}
```

| 字段 | 说明 |
|------|------|
| `clients` | 当前 WebSocket 连接数 |
| `history` | 内存中告警历史条数 |

### 告警规则

| 规则 | 条件 | 窗口 | 冷却 |
|------|------|------|------|
| CRIT 立即告警 | CRIT 级别日志 ≥1 | 1 分钟 | 30s |
| ERROR 突发 | ERROR 数量 >5 | 1 分钟 | 60s |
| 服务持续报错 | 单服务 ERROR >10 | 2 分钟 | 120s |

---

## 通用说明

### SSE 流式响应

所有 AI 分析端点均使用 Server-Sent Events：
- `Content-Type: text/event-stream`
- 每条消息格式：`data: <内容>\n\n`
- 流结束标记：`data: [DONE]\n\n`
- 客户端应使用 `EventSource` 或 `fetch` + `ReadableStream`

### CORS

所有端点均启用 CORS，允许任意来源跨域访问。

### API Key 安全

`OPENAI_API_KEY` 仅存储在服务端环境变量中，API 端点不接收、不返回、不转发 API Key。前端无法通过任何 API 获取 API Key。
