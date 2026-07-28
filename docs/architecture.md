# LogHawk 架构设计文档

## 概述

LogHawk 是一个运行在 Kubernetes 集群上的实时日志智能分析平台。它接收日志推送、通过消息队列削峰、结合 AI 进行异常检测，并通过 WebSocket 向前端推送实时告警。

## 架构图

```
                         ┌──────────────────────┐
                         │    浏览器 (Frontend)   │
                         │  HTML/CSS/JS + Chart.js │
                         └────┬────┬──────┬──────┘
                              │    │      │
                   ┌──────────┘    │      └──────────┐
                   ▼               ▼                  ▼
            ┌──────────┐   ┌──────────┐      ┌──────────┐
            │  Nginx   │   │  AI Proxy│      │  Alerter │
            │ 静态托管  │   │  Go :8003│      │ WebSocket│
            │ :30080   │   │ SSE 流式 │      │ 推送     │
            └──────────┘   └────┬─────┘      └────┬─────┘
                                │                  ▲
                                │ OpenAI/Ollama    │
                                │                  │
                          ┌─────┴──────────────────┴─────┐
                          │        K8s 集群内部           │
                          │                               │
   用户/应用 ──POST──▶ Ingest (Go) ──写──▶ RabbitMQ       │
                      :8001 ×2副本         消息削峰       │
                                              │           │
                                     ┌────────┴────────┐  │
                                     ▼                  ▼  │
                              Analyzer          AI Analyzer │
                              (规则引擎)        (LLM分析)   │
                                  │                  │      │
                                  └────┬─────────────┘      │
                                       ▼                    │
                                  PostgreSQL                │
                                  日志归档                  │
                                       │                    │
                                       ▼                    │
                                  Redis Pub/Sub             │
                                       │                    │
                                       └──────▶ Alerter     │
                                                WebSocket   │
                                                           │
                          ┌────────────────────────────────┤
                          │  Prometheus ──▶ Grafana        │
                          │  指标采集        仪表盘         │
                          └────────────────────────────────┘
```

## 服务详解

### 1. Ingest Service (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | 零外部依赖（标准库 only） |
| 副本 | 2（多节点分布） |
| 端口 | 8001 |
| 内存 | ~10MB 实际用量 |

**职责：**
- POST `/ingest` — 接收日志批量推送（JSON）
- GET `/logs` — 下游服务拉取日志（消费模式）
- GET `/logs/stream` — SSE 实时日志流推送
- GET `/health` — 健康检查
- GET `/stats` — 摄入统计

**设计要点：**
- 内存环形缓冲区（容量 10000 条），写满后覆盖最旧数据
- SSE 端点支持断线重连，重连后补齐增量数据
- 零外部依赖，`go build` 单文件部署，Docker 镜像 < 5MB
- 无状态设计，任意副本可独立处理请求

### 2. AI Proxy (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | 零外部依赖（标准库 only） |
| 副本 | 1-2 |
| 端口 | 8003 |
| 内存 | ~15MB 实际用量 |

**职责：**
- POST `/api/analyze` — 单次日志分析（无状态）
- POST `/api/chat` — 多轮对话分析（会话记忆）
- POST `/api/logs/ingest` — 日志摄入（供巡检使用）
- POST `/api/patrol/start` — 启动自主巡检
- POST `/api/patrol/stop` — 停止巡检
- GET `/api/patrol/status` — 巡检状态
- GET `/api/patrol/stream` — 巡检结果 SSE 流
- POST `/api/knowledge/reload` — 热重载知识库

**三大 Agent：**

| Agent | 功能 | 触发 |
|-------|------|------|
| 🔍 自主巡检 | 每 N 秒自动扫描日志，发现异常主动推送 | `/api/patrol/start` |
| 💬 多轮诊断 | 会话记忆 + 上下文追问 | `/api/chat` |
| 📚 RAG 知识库 | 加载 `knowledge/*.md` 增强分析 | 启动时自动加载 |

**安全设计：**
- API Key 存服务端环境变量，前端不可见
- AI 只建议命令，绝不自动执行
- 命令输出统一用 ````bash:copy` 格式，前端渲染复制按钮

### 3. Frontend (HTML/CSS/JS)

| 属性 | 值 |
|------|-----|
| 运行环境 | Nginx Alpine |
| 框架 | 纯 HTML/CSS/JS（无框架依赖） |
| 图表 | Chart.js CDN |
| 端口 | 80（NodePort 30080） |

**功能模块：**
- 实时日志流（终端风格，分级过滤）
- 告警中心（AI 分析卡片）
- 监控仪表盘（Chart.js 4 图表）
- AI 对话面板（SSE 流式、巡检开关、命令复制）
- 双主题：专业版 + 笨鹰版（5 次点击 Logo 切换）

### 4. 中间件

| 组件 | 镜像 | 用途 |
|------|------|------|
| PostgreSQL 16 | `postgres:16-alpine` | 日志归档、异常历史 |
| RabbitMQ 3.12 | `rabbitmq:3.12-alpine` | 消息削峰，Ingest→Analyzer 解耦 |
| Redis 7 | `redis:7-alpine` | Pub/Sub 实时推送、结果缓存 |

### 5. 可观测性

| 组件 | 端口 | 用途 |
|------|------|------|
| Prometheus | 9090 | 自定义指标采集（摄入速率、队列深度、异常检出率） |
| Grafana | 3000 (NodePort 30300) | 预配置仪表盘 |



### 6. Chaos Service (Go)

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.21+ |
| 依赖 | client-go (K8s API) |
| 副本 | 1 |
| 端口 | 8005 |
| RBAC | ServiceAccount + Role (deployments/pods/services/configmaps/networkpolicies) |

**职责：**
- POST /chaos/break — 注入故障（6 个预设场景）
- POST /chaos/verify — 验证用户是否修复
- GET /chaos/hint — 获取排查提示（3 级）
- GET /chaos/status — 查看当前故障状态
- POST /chaos/reset — 重置全部场景

**安全设计：**
- 所有操作限制在 loghawk namespace
- **Bearer Token 认证**：所有 `/chaos/*` 接口必须携带 `Authorization: Bearer <CHAOS_API_TOKEN>`
- 5 分钟自动回滚，防止用户"修不好"
- 验证逻辑检查真正修复（不仅 Pod Running，还检查镜像/配置/Endpoints）

## 数据流

```
1. 用户 POST /ingest → Ingest 写入环形缓冲区
2. Ingest 异步写入 RabbitMQ（削峰）
3. Analyzer 消费 RabbitMQ → 规则匹配 → 写 PostgreSQL
4. AI Analyzer 消费 RabbitMQ → LLM 分析 → 发布 Redis Pub/Sub
5. Alerter 订阅 Redis → WebSocket 推前端
6. 用户可在前端 AI 面板直接提问 → AI Proxy → OpenAI/Ollama → SSE 流式返回
7. Prometheus 抓取各服务指标 → Grafana 可视化
```

## 故障自愈

| 场景 | 机制 |
|------|------|
| Ingest Pod 宕机 | K8s 自动重启/漂移，对端透明 |
| Worker 节点宕机 | Pod 调度到存活节点，队列消息不丢失 |
| 队列堆积 | Analyzer 扩容消费，30 秒消化积压 |
| WebSocket 断开 | 前端 3 秒自动重连，重连后补齐历史 |
| AI Proxy 宕机 | 巡检任务丢失（可接受），前端自动重连 SSE 流 |
