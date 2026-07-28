# LogHawk 开发指南

## 项目结构

```
LogHawk/
├── services/
│   ├── ingest/              # Go — 日志摄入服务
│   │   ├── main.go          # 单文件，~180 行
│   │   ├── go.mod
│   │   └── Dockerfile       # 多阶段构建，FROM scratch
│   ├── ai-proxy/            # Go — AI 分析代理
│   │   ├── main.go          # 单文件，~500 行
│   │   ├── go.mod
│   │   ├── Dockerfile       # 多阶段构建，FROM scratch
│   │   ├── deploy.yaml      # K8s 部署文件
│   │   └── knowledge/       # RAG 知识库
│   └── frontend/            # 纯前端
│       ├── index.html       # 单文件应用（~60KB）
│       ├── nginx.conf
│       ├── Dockerfile
│       └── assets/          # Logo + 吉祥物图片
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

| 组件 | 语言 | 依赖 | 构建产物 |
|------|------|------|---------|
| Ingest | Go 1.21+ | 标准库 only | 单二进制 ~4MB |
| AI Proxy | Go 1.21+ | 标准库 only | 单二进制 ~5MB |
| Frontend | HTML/CSS/JS | Chart.js CDN | 静态文件 |

**设计原则：**
- Go 服务零外部依赖，`go build` 即用
- Docker 镜像 `FROM scratch`，< 6MB
- 前端纯原生，无框架，兼容所有浏览器

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

| 代码段 | 行 | 功能 |
|--------|-----|------|
| CSS Variables | L10-80 | 双主题 CSS 变量 |
| `.theme-goofy` | L85-130 | 笨鹰主题覆盖 |
| Log Terminal | L150-250 | 终端风格日志流 |
| Alert Cards | L260-320 | 告警卡片 + AI 分析 |
| Charts | L400-480 | Chart.js 4 图表 |
| AI Panel | L500-600 | AI 对话面板 |
| Theme Toggle | JS L20-50 | 5 次点击切换主题 |
| Log Generation | JS L80-150 | 模拟日志生成 |
| Patrol Agent | JS L200-300 | 巡检开关 + SSE 监听 |
| Command Copy | JS L350-400 | bash:copy 渲染 + 复制 |

---

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
