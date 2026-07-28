# LogHawk 架构设计文档

## 概述

LogHawk 是一个运行在 Kubernetes 集群上的实时日志智能分析平台。它通过 DaemonSet 采集节点日志、经消息队列削峰、由规则引擎实时触发告警，并通过 WebSocket/HTTP 轮询向前端推送。

## 实际架构（当前部署状态）

```
                        ┌──────────────────────────┐
                        │    浏览器 (Frontend)       │
                        │  HTML/CSS/JS + Chart.js    │
                        │  http://IP:30080          │
                        └──────┬──────┬──────┬──────┘
                               │      │      │
                    ┌──────────┘      │      └──────────┐
                    ▼                 ▼                  ▼
             ┌──────────┐   ┌──────────────┐   ┌──────────────┐
             │  Nginx   │   │  AI Proxy    │   │  Alerter     │
             │  静态托管 │   │  Go :8003    │   │  Go :8004    │
             │  API代理  │   │  SSE 流式    │   │  WS + REST   │
             └────┬─────┘   │  LLM 对话    │   │  规则引擎    │
                  │         └──────┬───────┘   └──────┬───────┘
                  │                │                   ▲
                  │          OpenAI/DeepSeek           │
                  │                                    │
           ┌──────┴────────────────────────────────────┴───────┐
           │                  K8s 集群内部                      │
           │                                                    │
           │  log-collector (DaemonSet ×3)                       │
           │  每节点采集 /var/log/containers/*.log                │
           │       │                                            │
           │       ▼                                            │
           │  Ingest (Go :8001 ×2副本)                           │
           │  环形缓冲区(10000条) + SSE流推送                     │
           │       │                                            │
           │       ├──▶ RingBuffer ──▶ GET /logs (前端轮询)      │
           │       │              ──▶ GET /stats (统计API)       │
           │       │                                            │
           │       └──▶ RabbitMQ "logs" 队列                     │
           │                │                                   │
           │                ▼                                   │
           │         Alerter 规则引擎                             │
           │         滑动窗口 + 3条规则:                           │
           │         ① CRIT → 立即告警 (30s冷却)                 │
           │         ② ERROR >5/分钟 → 突发告警 (60s冷却)         │
           │         ③ 单服务 >10 ERROR/2分钟 → 服务告警 (120s)   │
           │                │                                   │
           │                ▼                                   │
           │         GET /alerts ──▶ 前端 Alert 面板              │
           │                                                    │
           │  Chaos (Go :8005)                                   │
           │  6个故障场景 + Bearer Token认证 + 5分钟自动恢复        │
           └────────────────────────────────────────────────────┘
```

## 服务详解

### 1. Ingest Service (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | rabbitmq/amqp091-go |
| 副本 | 2（多节点分布） |
| 端口 | 8001 |
| 镜像大小 | ~8MB |

**职责：**
- POST `/ingest` — 接收日志批量推送（JSON）
- GET `/logs` — 返回最近 500 条日志
- GET `/logs/stream` — SSE 实时日志流推送
- GET `/stats` — 摄入统计（总量、队列深度、运行时间）
- GET `/metrics` — Prometheus 格式指标
- GET `/health` — 健康检查

**设计要点：**
- 内存环形缓冲区（容量 10000 条），写满后覆盖最旧数据
- 收到日志后异步推送到 RabbitMQ `logs` 队列（非阻塞，连接失败不影响摄入）
- RabbitMQ 不可用时优雅降级，日志摄入照常工作
- SSE 端点支持断线重连

### 2. AI Proxy (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | 标准库 only |
| 副本 | 1 |
| 端口 | 8003 |

**职责：**
- POST `/api/analyze` — 单次日志分析（无状态）
- POST `/api/chat` — 多轮对话分析（会话记忆）
- POST `/api/patrol/start` — 启动自主巡检
- POST `/api/patrol/stop` — 停止巡检
- GET `/api/patrol/stream` — 巡检结果 SSE 流
- POST `/api/knowledge/reload` — 热重载知识库

**三大 Agent：**
| Agent | 功能 | 触发 |
|-------|------|------|
| 🔍 自主巡检 | 每 N 秒自动扫描日志 | `/api/patrol/start` |
| 💬 多轮诊断 | 会话记忆 + 上下文追问 | `/api/chat` |
| 📚 RAG 知识库 | 加载 `knowledge/*.md` | 启动时自动加载 |

**安全设计：** API Key 存服务端环境变量，前端不可见

### 3. Alerter (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | rabbitmq/amqp091-go, websocket |
| 副本 | 1 |
| 端口 | 8004 |

**职责：**
- 消费 RabbitMQ `logs` 队列
- 滑动窗口规则引擎（2 分钟窗口）
- 3 条告警规则：CRIT 立即告警 / ERROR 突发 / 单服务持续报错
- 冷却机制防止告警风暴（30s-120s 不等）
- WebSocket `/ws` — 实时推送告警到前端
- GET `/alerts` — 前端轮询获取历史告警（HTTP 备选）
- POST `/push` — 手动推送告警
- 连接断开自动重连 RabbitMQ

### 4. Log Collector (Go, DaemonSet)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 副本 | 每节点 1 个 |
| 权限 | root (UID 0)，需读取宿主机 /var/log |

**职责：**
- 挂载宿主机 `/var/log/containers/` 目录
- 每 5 秒采集容器日志变更
- 批量 POST 到 Ingest 服务

### 5. Chaos Service (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | client-go (K8s API) |
| 副本 | 1 |
| 端口 | 8005 |
| RBAC | ServiceAccount + Role |

**职责：** 注入 6 类 K8s 故障（镜像篡改/Service Selector/ConfigMap/副本归零/NetworkPolicy/磁盘填充），5 分钟自动恢复，Bearer Token 认证。

### 6. Frontend (HTML/CSS/JS)

| 属性 | 值 |
|------|-----|
| 运行环境 | Nginx Alpine |
| 框架 | 纯 HTML/CSS/JS（无框架依赖） |
| 图表 | Chart.js（本地引用，无 CDN 依赖） |
| 端口 | 80（NodePort 30080） |

**功能模块：**
- 实时日志流（终端风格，4 级过滤）— 每 3s 轮询 `/api/ingest/logs`
- 告警中心 — 每 5s 轮询 `/api/alerter/alerts`
- 监控仪表盘（Chart.js 4 图）— 每 5s 轮询 `/api/ingest/stats`
- AI 对话面板（SSE 流式、巡检开关）
- 故障演练面板（6 场景 Break/Verify/Hint/Reset）
- nginx 反向代理：`/api/ingest/`→ingest:8001, `/api/alerter/`→alerter:8004, `/api/`→ai-proxy:8003

### 7. 中间件

| 组件 | 镜像 | 用途 | 端口 |
|------|------|------|------|
| RabbitMQ 3.12 | `rabbitmq:3.12-alpine` | 消息削峰，Ingest→Alerter 解耦 | 5672 |
| PostgreSQL 16 | `postgres:16-alpine` | 预留：日志归档 | 5432 |
| Redis 7 | `redis:7-alpine` | 预留：缓存/Pub/Sub | 6379 |
| Prometheus | `prom/prometheus` | 指标采集 | 9090 |
| Grafana | `grafana/grafana` | 仪表盘 | 3000 (NodePort 30300) |

## 数据流（当前实现）

```
1. log-collector 每 5s 采集节点日志 → POST /ingest
2. Ingest 写入 RingBuffer + 异步推 RabbitMQ "logs" 队列
3. 前端每 3s GET /api/ingest/logs → 实时展示
4. 前端每 5s GET /api/ingest/stats → 统计卡
5. Alerter 消费 RabbitMQ → 规则引擎匹配 → 生成告警
6. 前端每 5s GET /api/alerter/alerts → 告警面板
```

## 关键安全设计

- Chaos 服务 Bearer Token 认证（`CHAOS_API_TOKEN`）
- AI Proxy API Key 存服务端，前端不可见
- CORS 动态检查 Origin（非 `*` 通配符）
- Ingest 请求体限 10MB（`http.MaxBytesReader`）
- AI Proxy HTTP 客户端超时：连接 10s / TLS 10s / 空闲 90s
- K8s API 调用 15s 超时（Chaos 服务）
- 容器镜像使用具体 tag，非 `latest`
