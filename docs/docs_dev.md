# LogHawk 开发指南

## 项目结构

```
LogHawk/
├── services/
│   ├── ingest/              # Go — 日志摄入服务
│   │   ├── main.go          # ~430行，含 RingBuffer + RabbitMQ 发布
│   │   ├── go.mod
│   │   └── Dockerfile
│   ├── ai-proxy/            # Go — AI 分析代理
│   │   ├── main.go          # ~580行，3 Agent + SSE + RAG
│   │   ├── go.mod
│   │   ├── Dockerfile
│   │   └── knowledge/       # RAG 知识库
│   ├── alerter/             # Go — 告警引擎 ⭐新增
│   │   ├── main.go          # ~420行，RabbitMQ 消费 + 规则引擎 + WebSocket
│   │   ├── go.mod
│   │   └── Dockerfile
│   ├── chaos/               # Go — 故障注入
│   │   ├── main.go          # K8s API 客户端 + 6 场景
│   │   ├── go.mod
│   │   └── Dockerfile
│   ├── log-collector/       # Go — 节点日志采集 DaemonSet
│   │   ├── main.go
│   │   ├── go.mod
│   │   └── Dockerfile
│   └── frontend/            # 纯前端
│       ├── index.html       # 单文件应用（~3400行）
│       ├── nginx.conf       # 3 路 API 反向代理
│       ├── Dockerfile
│       └── assets/          # Logo + 吉祥物 + Chart.js 本地副本
├── k8s/                     # Kubernetes YAML（16 个文件）
├── scripts/                 # 部署脚本
├── docs/                    # 技术文档
├── charts/loghawk/          # Helm Chart
├── monitoring/              # Grafana 仪表盘
├── docker-compose.yaml      # 本地开发环境
├── Makefile                 # 统一构建
└── Vagrantfile              # 3 节点 VM 配置
```

---

## 技术栈

| 组件 | 语言 | 依赖 | 端口 |
|------|------|------|------|
| Ingest | Go 1.21+ | rabbitmq/amqp091-go | 8001 |
| AI Proxy | Go 1.21+ | 标准库 only | 8003 |
| Alerter | Go 1.21+ | rabbitmq/amqp091-go, golang.org/x/net/websocket | 8004 |
| Chaos | Go 1.21+ | client-go (K8s API) | 8005 |
| Log Collector | Go 1.21+ | 标准库 only | — |
| Frontend | HTML/CSS/JS | Chart.js（本地引用） | 80 |

**设计原则：**
- Go 服务尽量零外部依赖，`go build` 即用
- 前端纯原生，无框架，Chart.js 本地化无 CDN 依赖
- Docker 镜像使用具体 tag，非 `latest`

---

## 本地开发

### 环境准备

```bash
# Go
snap install go --classic

# Docker
curl -fsSL https://get.docker.com | sh

# 克隆
git clone https://github.com/linwumu/LogHawk.git
cd LogHawk
```

### 启动开发环境

```bash
# 启动中间件
docker-compose up -d postgres rabbitmq redis

# 编译 Ingest
cd services/ingest && go run . &
# 或: go build -o ingest . && ./ingest &

# 编译 AI Proxy
cd services/ai-proxy && go run . &

# 前端用 Python HTTP Server
cd services/frontend && python3 -m http.server 30080 &
```

### 本地开发注意事项

前端在 `localhost` 环境下会自动启用以下守卫，避免控制台报错：

- **Chaos 服务守卫**：`chaosReachable()` 检测到 `localhost` + 默认 K8s DNS 端点（`*.loghawk`）时跳过 fetch，避免 `net::ERR_NAME_NOT_RESOLVED`
- **端点配置为空**：Chaos API 端点输入框默认为空，仅用 placeholder 提示，需在 K8s 集群环境手动配置
- **AI Proxy 端点空值处理**：AI Endpoint 输入框默认为空，K8s 环境留空会自动使用相对路径 `/api/*`（由前端 Nginx 代理到 ai-proxy）
- **localStorage 自动清理**：本地运行时自动清除残留的 K8s DNS 端点配置（包括 AI 和 Chaos 端点）
- **AI Proxy 连接失败提示**：`/api/patrol/*` 请求失败时显示 toast 提示而非静默错误
- **HTML 缓存控制**：`<meta http-equiv="Cache-Control" content="no-cache, no-store, must-revalidate">` 防止浏览器使用旧版本

> 如需在本地测试 Chaos 服务，将端点改为 `http://localhost:8005`（需先本地启动 Chaos 服务）。

### Makefile

```bash
make build        # 编译所有 Go 服务
make up           # docker-compose 启动全部
make down         # 停止
make logs         # 查看日志
make test         # 运行测试
make clean        # 清理编译产物
```

---

## 代码导航

### Ingest Service (`services/ingest/main.go`)

| 函数 | 行 | 职责 |
|------|-----|------|
| `main()` | L150+ | 注册路由，启动 HTTP 服务 |
| `handleIngest()` | L80+ | POST /ingest 处理 |
| `handleGetLogs()` | L105+ | GET /logs 处理 |
| `handleLogStream()` | L115+ | GET /logs/stream SSE 处理 |
| `RingBuffer` | L30+ | 线程安全环形缓冲区 |

**关键设计：**
- 环形缓冲区容量 10000 条，写满覆盖最旧
- SSE 端点对比上次推送位置，只推增量
- 零锁竞争：读写分离锁

### AI Proxy (`services/ai-proxy/main.go`)

| 模块 | 行 | 职责 |
|------|-----|------|
| `LogBuffer` | L35+ | 日志环形缓冲（供巡检） |
| `PatrolScheduler` | L65+ | 巡检调度器（goroutine + ticker） |
| `SessionStore` | L140+ | 会话记忆（内存 Map） |
| `KnowledgeBase` | L165+ | 知识库加载与热重载 |
| `buildSystemPrompt()` | L200+ | 专业模式 System Prompt |
| `buildGoofySystemPrompt()` | L250+ | 笨鹰暴躁老哥 Prompt |
| `buildPatrolMessages()` | L290+ | 巡检分析 Prompt 构建 |
| `streamAIWithHistory()` | L380+ | 多轮对话 + 会话持久化 |

### Frontend (`services/frontend/index.html`)

| 代码段 | 功能 |
|--------|------|
| CSS Variables | 双主题 CSS 变量（light/dark/goofy） |
| Log Terminal | 终端风格日志流，4 级过滤 |
| Alert Cards | 告警卡片 + AI 分析建议 |
| Charts | Chart.js 4 图表（摄入趋势/级别分布/延迟/异常） |
| AI Panel | AI 对话面板 + SSE 流式 + 巡检开关 |
| Chaos Grid | 6 场景故障演练面板 |
| fetchRealStats() | 每 5s 轮询 `/api/ingest/stats` |
| fetchLogs() | 每 3s 轮询 `/api/ingest/logs` ⭐新增 |
| fetchAlerts() | 每 5s 轮询 `/api/alerter/alerts` ⭐新增 |

### Alerter (`services/alerter/main.go`) ⭐新增

| 代码段 | 功能 |
|--------|------|
| `consumeRabbitMQ()` | RabbitMQ 消费者 + 自动重连 |
| `RuleEngine.feed()` | 滑动窗口规则匹配（2分钟窗口） |
| `RuleEngine.shouldAlert()` | 冷却机制防告警风暴 |
| `Hub.broadcast()` | WebSocket 多客户端广播 |
| `handleGetAlerts()` | GET /alerts 历史告警 API |

### Ingest (`services/ingest/main.go`)

| 代码段 | 功能 |
|--------|------|
| `RingBuffer` | 线程安全环形缓冲区（10000条） |
| `handleIngest()` | POST /ingest + RabbitMQ 异步发布 ⭐更新 |
| `handleLogStream()` | SSE 实时日志流 |
| `initRabbitMQ()` | RabbitMQ 连接（10次重试） ⭐新增 |
| `publishToQueue()` | 异步推送到 "logs" 队列 ⭐新增 |

---

## 开发日志

### 2026-07-28 — v1.0 功能完善

1. **前端假数据清理** — 移除硬编码模拟日志、告警、统计数据，改为从后端 API 轮询
2. **Chart.js 本地化** — 下载到 `assets/chart.umd.min.js`，消除 CDN 依赖（国内网络兼容性）
3. **日志实时展示** — 新增 `fetchLogs()`，每 3s 轮询 `/api/ingest/logs`
4. **nginx 多路代理** — 新增 `/api/ingest/`（→ingest:8001）、`/api/alerter/`（→alerter:8004）
5. **告警链路打通** — ingest 异步发布到 RabbitMQ `logs` 队列，alerter 消费 + 规则引擎
6. **3 条告警规则** — CRIT 立即告警 / ERROR 突发（>5/min）/ 单服务持续报错（>10/2min）
7. **前端告警轮询** — 新增 `fetchAlerts()`，每 5s 轮询 `/api/alerter/alerts`
8. **各模块 try-catch 防护** — 单模块崩溃不影响整体页面运行
9. **默认不暂停** — `isPaused: false`，打开页面即展示日志流
10. **RABBITMQ_PORT 冲突修复** — 改为 `RABBITMQ_AMQP_PORT`，避免 K8s Service 自动注入的 `tcp://` 值污染

### 踩坑记录

| 问题 | 根因 | 解决 |
|------|------|------|
| Chart.js 加载失败页面全崩 | CDN 国内被墙 | 本地引用 `assets/chart.umd.min.js` |
| nginx `/api/ingest/` 不代理 | Docker 层缓存 `COPY nginx.conf` | `--no-cache` 构建 |
| 终端粘贴反引号污染 nginx.conf | SSH 终端自动转义 URL | scp 二进制传输 |
| 前端日志不显示 | `isPaused: true` 默认暂停 | 改为 `false` |
| RabbitMQ `guest` 登录拒绝 | 3.3+ 禁止 guest 远程登录 | `rabbitmqctl` 创建 admin 用户 |
| `RABBITMQ_PORT` 被 K8s 污染 | K8s 自动注入 `tcp://IP:PORT` | 改名为 `RABBITMQ_AMQP_PORT` |
| Secret 密码占位符 | `<CHANGE_ME_*>` 含特殊字符 | 重建 Secret + rabbitmqctl 改密 |
| 镜像 tag 不匹配 | Docker build 用 short name | retag 为完整 registry 路径 |
| `defer cancel()` in for loop | context 泄漏 | 改为显式 `cancel()` |

## 修改指南

### 添加新日志模板

编辑 `services/frontend/index.html` 中的 `LOGS` 对象：

```javascript
var LOGS = {
  INFO: ["你的 INFO 日志模板..."],
  WARN: ["你的 WARN 日志模板..."],
  ...
}
```

### 添加新知识库

```bash
vim services/ai-proxy/knowledge/my-guide.md
curl -X POST http://localhost:8003/api/knowledge/reload
```

### 调整巡检间隔

```bash
export PATROL_INTERVAL_SEC=60  # 改为 60 秒
./ai-proxy
```

### 修改 AI 人格

编辑 `services/ai-proxy/main.go` 中的 `buildGoofySystemPrompt()` 函数。

---

## 构建 Docker 镜像

```bash
# Ingest
cd services/ingest
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ingest .
docker build -t loghawk/ingest:v1.0.0 .

# AI Proxy
cd services/ai-proxy
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ai-proxy .
docker build -t loghawk/ai-proxy:v1.0.0 .

# Frontend
cd services/frontend
docker build -t loghawk/frontend:v1.0.0 .
```

镜像大小参考：
| 镜像 | 大小 |
|------|------|
| loghawk/ingest | ~4.5 MB |
| loghawk/ai-proxy | ~5.5 MB |
| loghawk/frontend | ~12 MB |

---

## 测试

```bash
# 测试 Ingest
curl -X POST http://localhost:8001/ingest \
  -H "Content-Type: application/json" \
  -d '[{"timestamp":"12:00:00.000","level":"INFO","message":"test"}]'

# 测试 AI Proxy
curl -X POST http://localhost:8003/health

# 测试巡检
curl -X POST http://localhost:8003/api/patrol/start
curl http://localhost:8003/api/patrol/status
```

---

## 提交规范

```
feat: 新功能
fix: 修复 Bug
docs: 文档更新
refactor: 重构
chore: 构建/工具
style: 格式调整
```
