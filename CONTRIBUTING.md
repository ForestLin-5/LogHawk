# Contributing to LogHawk

## 快速开始

```bash
# 1. Fork + Clone
git clone https://github.com/YOUR_USERNAME/LogHawk.git
cd LogHawk

# 2. 本地开发
make build        # 编译所有 Go 服务
make up           # docker-compose 一键启动
# 打开 http://localhost:30080

# 3. 修改代码
# services/ingest/      — Go 日志摄入
# services/ai-proxy/    — Go AI 分析代理
# services/frontend/    — 前端 (纯 HTML/JS)

# 4. 提交 PR
git checkout -b feat/my-feature
git commit -m "feat: add xxx"
git push origin feat/my-feature
```

## 项目结构

```
LogHawk/
├── services/
│   ├── ingest/          # Go — 日志摄入 (零外部依赖)
│   ├── ai-proxy/        # Go — AI 分析代理 (零外部依赖)
│   └── frontend/        # 纯 HTML/CSS/JS — 运维监控界面
├── k8s/                 # Kubernetes 部署文件
├── scripts/             # 部署 & 故障演示脚本
└── docs/                # 架构 & 部署文档
```

## 技术栈

| 组件 | 语言 | 依赖 |
|------|------|------|
| Ingest Service | Go 1.21+ | 标准库 only |
| AI Proxy | Go 1.21+ | 标准库 only |
| Frontend | HTML/CSS/JS | Chart.js CDN |
| 中间件 | PostgreSQL / RabbitMQ / Redis | Docker |

## Commit 规范

- `feat:` 新功能
- `fix:` 修复
- `docs:` 文档
- `refactor:` 重构
- `chore:` 杂项

## AI Proxy 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `OPENAI_API_KEY` | API 密钥 | (必填) |
| `OPENAI_BASE_URL` | API 端点 | `https://api.openai.com/v1` |
| `OPENAI_MODEL` | 模型名称 | `gpt-4o-mini` |
| `AI_PROXY_PORT` | 监听端口 | `8003` |
| `PATROL_INTERVAL_SEC` | 巡检间隔 | `30` |
