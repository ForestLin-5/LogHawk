# LogHawk 配置参考

## Ingest Service

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `INGEST_PORT` | `8001` | HTTP 监听端口 |
| `LOG_LEVEL` | `info` | 日志级别（仅影响 stdout 输出） |

### 环形缓冲区

缓冲区容量硬编码为 `10000` 条，位于 `main.go` 中 `NewRingBuffer(10000)`。修改需重新编译。

---

## AI Proxy

### 必需环境变量

| 环境变量 | 说明 | 示例 |
|----------|------|------|
| `OPENAI_API_KEY` | AI API 密钥 | `sk-abc123...` |

### 可选环境变量

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | API 端点（切换 Ollama/通义千问） |
| `OPENAI_MODEL` | `gpt-4o-mini` | 模型名称 |
| `AI_PROXY_PORT` | `8003` | HTTP 监听端口 |
| `PATROL_INTERVAL_SEC` | `30` | 巡检间隔（秒） |
| `KNOWLEDGE_DIR` | `knowledge` | 知识库目录路径 |
| `AI_PROXY_SYSTEM_PROMPT` | （内置默认值） | 自定义 System Prompt |

### 常用后端配置

```bash
# OpenAI 默认
export OPENAI_API_KEY=sk-xxx

# Ollama 本地（免费）
export OPENAI_BASE_URL=http://localhost:11434/v1
export OPENAI_API_KEY=ollama
export OPENAI_MODEL=qwen2.5:7b

# 阿里通义千问
export OPENAI_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
export OPENAI_API_KEY=sk-xxx

# DeepSeek
export OPENAI_BASE_URL=https://api.deepseek.com/v1
export OPENAI_API_KEY=sk-xxx
```

---

## K8s ConfigMap

### loghawk-config

```yaml
data:
  LOG_LEVEL: "info"
  INGEST_PORT: "8001"
  AI_PROXY_PORT: "8003"
  ALERTER_PORT: "8004"
  CHAOS_PORT: "8005"
  RABBITMQ_HOST: "rabbitmq.loghawk"
  RABBITMQ_PORT: "5672"
  RABBITMQ_QUEUE: "log-stream"
  POSTGRES_HOST: "postgres.loghawk"
  POSTGRES_PORT: "5432"
  POSTGRES_DB: "loghawk"
  REDIS_HOST: "redis.loghawk"
  REDIS_PORT: "6379"
```

### loghawk-secret

```yaml
stringData:
  POSTGRES_USER: "loghawk"
  POSTGRES_PASSWORD: "<CHANGE_ME_PASSWORD>"
  RABBITMQ_USER: "admin"
  RABBITMQ_PASS: "<CHANGE_ME_PASSWORD>"
  OPENAI_API_KEY: "<CHANGE_ME_OPENAI_API_KEY>"
  CHAOS_API_TOKEN: "<CHANGE_ME_CHAOS_API_TOKEN>"
```

### ai-proxy-secret

```yaml
stringData:
  OPENAI_API_KEY: "sk-your-key-here"
  OPENAI_BASE_URL: "https://api.openai.com/v1"
  OPENAI_MODEL: "gpt-4o-mini"
```

---

## Docker Compose

所有环境变量在 `docker-compose.yaml` 中配置：

```yaml
services:
  ai-proxy:
    environment:
      - OPENAI_API_KEY=${OPENAI_API_KEY:-}
      - OPENAI_BASE_URL=${OPENAI_BASE_URL:-https://api.openai.com/v1}
      - OPENAI_MODEL=${OPENAI_MODEL:-gpt-4o-mini}
      - PATROL_INTERVAL_SEC=30
```

使用 `.env` 文件（不会被 Git 跟踪）：
```bash
# .env
OPENAI_API_KEY=sk-xxx
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_MODEL=gpt-4o-mini
```

---

## 资源限制

### K8s ResourceQuota

```yaml
spec:
  hard:
    requests.cpu: "2"
    requests.memory: "2Gi"
    limits.cpu: "4"
    limits.memory: "4Gi"
    pods: "20"
```

### 各服务资源占用

| 服务 | Request Mem | Limit Mem | Request CPU | Limit CPU |
|------|-------------|-----------|-------------|-----------|
| Ingest | 16Mi | 64Mi | 10m | 100m |
| AI Proxy | 32Mi | 128Mi | 20m | 200m |
| Frontend | 16Mi | 32Mi | 10m | 50m |
| PostgreSQL | 128Mi | 256Mi | 100m | 500m |
| RabbitMQ | 128Mi | 256Mi | 100m | 500m |
| Redis | 64Mi | 128Mi | 50m | 200m |
| Prometheus | 256Mi | 512Mi | 100m | 500m |
| Grafana | 128Mi | 256Mi | 50m | 200m |

---


## PriorityClass

| PriorityClass | Value | 用途 |
|---------------|-------|------|
| `loghawk-high` | 1,000,000 | 核心服务（Ingest, AI Proxy）— 内存压力时最后被驱逐 |
| `loghawk-low` | 100 | 辅助服务 — 可优先驱逐 |
| （默认） | 0 | K8s 内置默认值 |

使用方式：
```yaml
spec:
  priorityClassName: loghawk-high
```

## 网络策略

`k8s/networkpolicy.yaml` 定义了命名空间内的流量规则：

- Ingress → 允许来自 `k8s-worker*` 的 TCP 8001
- PostgreSQL → 仅允许来自 Ingest/Analyzer 的 TCP 5432
- RabbitMQ → 允许来自 Ingest 的 TCP 5672
- Redis → 允许来自 Analyzer 的 TCP 6379

默认拒绝所有入站流量（deny-all）。
