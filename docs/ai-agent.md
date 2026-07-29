# LogHawk AI Agent 系统文档

## 概述

LogHawk AI Agent 是一个运行在 AI Proxy (Go) 服务中的三合一智能分析系统。所有 Agent 共享同一个 OpenAI 兼容 API 端点，通过不同的 System Prompt 和调用模式实现不同功能。

## 架构

```
┌─────────────────────────────────────────┐
│              AI Proxy (Go :8003)         │
│                                          │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ │
│  │ 巡检调度器│ │ 会话管理器│ │ 知识库   │ │
│  │ 30s Ticker│ │ Map存储  │ │ .md加载  │ │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘ │
│       │            │            │        │
│       └────────────┼────────────┘        │
│                    │                     │
│              ┌─────┴─────┐               │
│              │ System    │               │
│              │ Prompt    │               │
│              │ Builder   │               │
│              └─────┬─────┘               │
│                    │                     │
│              ┌─────┴─────┐               │
│              │ OpenAI    │               │
│              │ Compatible│               │
│              │ API Call  │               │
│              └───────────┘               │
└─────────────────────────────────────────┘
```

## Agent 1: 自主巡检 (Patrol Agent)

### 工作原理

1. 前端定期调用 `POST /api/logs/ingest` 推送日志到服务端环形缓冲区
2. 后台 goroutine 每 N 秒（默认 30s）触发一次巡检
3. 获取缓冲区最近 100 条日志 → 构建分析 Prompt → 调用 AI
4. 分析结果通过 SSE 流推送到所有连接的巡检前端
5. 前端在 AI 对话面板中显示巡检发现

### 巡检 Prompt 结构

```
【自动巡检】请分析以下 N 条最新日志：
- 🟢 健康评估（一句话总结）
- 🔴 发现的问题（按严重程度排列）
- 💡 建议操作（命令格式：```bash:copy）
- 📋 日志摘要
```

### 配置

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `PATROL_INTERVAL_SEC` | 30 | 巡检间隔（秒） |

### 状态存储

巡检结果不持久化。日志缓冲区在服务重启后清空。

---

## Agent 2: 多轮诊断 (Chat Agent)

### 工作原理

1. 前端发送 `POST /api/chat`，携带 `session_id`
2. 服务端从 `SessionStore`（内存 Map）获取该会话的历史消息
3. 将历史消息 + 新问题 + 日志上下文拼接为完整 Prompt
4. 调用 AI → SSE 流式返回
5. 将用户问题和 AI 回复追加到会话历史（最多 30 条）

### 会话生命周期

- 会话存储在内存中，服务重启后清空
- 每个会话最多保留 30 条消息（15 轮对话）
- 超出后自动淘汰最旧消息
- 不支持跨 Pod 共享（无状态设计）

### 使用示例

```
用户: "帮我看看最近的错误"
AI: "检测到 3 个连接池耗尽错误..."
用户: "这个问题跟昨天的磁盘告警有关吗？"
AI: "根据上下文分析，两者无直接关联..."  ← 记得第一轮的内容
```

---

## Agent 3: RAG 运维知识库

### 工作原理

1. 服务启动时扫描 `knowledge/` 目录下所有 `.md` 文件
2. 将文件内容拼接为知识上下文
3. 注入到 System Prompt 末尾
4. AI 分析时自动参考知识库内容
5. 支持热重载：`POST /api/knowledge/reload`

### 文件结构

```
services/ai-proxy/knowledge/
├── common-issues.md        # 常见故障排查手册
└── k8s-troubleshooting.md  # K8s 排障速查
```

### 知识库编写规范

- 使用 Markdown 格式
- 每个文件专注于一个主题领域
- 包含：症状 → 根因 → 修复步骤 → 验证命令
- 文件名即知识领域，AI 会根据问题相关性引用

---

## 安全设计

### API Key 隔离

```
浏览器 ──HTTPS──▶ AI Proxy ──Bearer API Key──▶ OpenAI
                   (服务端)                    
                                              
❌ 浏览器永远看不到 API Key                      
✅ API Key 只存在于服务端环境变量                 
✅ 前端无法通过任何 API 端点获取 Key              
```

### 命令执行管控

```
AI 输出: ```bash:copy
         kubectl scale deploy ingest --replicas=3
         ```

前端渲染: ┌──────────────────────────────┐
         │ 💡 建议执行命令（人工审核） [📋 复制] │
         │ kubectl scale deploy ingest ...    │
         └──────────────────────────────┘

用户操作: 点击 [📋 复制] → 粘贴到终端 → 人工审核 → Enter 执行
```

**AI 绝不会自动执行任何命令。**

---

## 双人格系统

### 专业模式（默认）

```
System Prompt: 专业运维助手
语气: 冷静、准确、结构化
输出: 问题 → 根因 → 建议 → 命令
```

### 笨鹰模式（5 次点击 Logo）

```
System Prompt: 暴躁老哥
语气: 粗口、高抽象比喻、攻击性幽默
输出: 骂一句 → 分析 → 建议 → 嘴贱收尾
技术正确性: 100%（骂归骂，方案全对）
```

通过请求参数 `"goofy": true` 切换。

---

## 扩展

### 添加新知识库

```bash
# 1. 写 .md 文件
vim services/ai-proxy/knowledge/database-tuning.md

# 2. 热重载（无需重启服务）
curl -X POST http://localhost:8003/api/knowledge/reload
```

### 切换 AI 后端

```bash
# OpenAI
export OPENAI_API_KEY=sk-xxx

# Ollama（本地免费）
export OPENAI_BASE_URL=http://localhost:11434/v1
export OPENAI_API_KEY=ollama
export OPENAI_MODEL=qwen2.5:7b

# 阿里通义千问
export OPENAI_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
export OPENAI_API_KEY=sk-xxx
```
