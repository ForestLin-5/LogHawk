# LogHawk 部署指南

## 环境要求

| 资源 | 最低 | 推荐 |
|------|------|------|
| CPU | 2 核 | 4 核 |
| 内存 | 4 GB | 8 GB |
| 磁盘 | 20 GB | 50 GB |
| OS | Linux (Ubuntu 20.04+) | Ubuntu 22.04 |

---


## 方式 0: Helm Chart（最简单）

```bash
# 安装
helm install loghawk ./charts/loghawk -n loghawk --create-namespace

# 自定义配置
helm install loghawk ./charts/loghawk -n loghawk --create-namespace \
  --set aiProxy.env.openaiApiKey=sk-xxx \
  --set frontend.nodePort=30080

# 查看状态
helm list -n loghawk
kubectl -n loghawk get pods

# 卸载
helm uninstall loghawk -n loghawk
```

---

## 方式 1: Docker Compose（本地开发/演示）

最简单的方式，单机运行所有服务。

```bash
# 克隆项目
git clone https://github.com/linwumu/LogHawk.git
cd LogHawk

# 一键启动
docker-compose up -d

# 查看状态
docker-compose ps

# 打开前端
open http://localhost:30080

# 查看日志
docker-compose logs -f
```

| 服务 | 地址 |
|------|------|
| 前端 | http://localhost:30080 |
| AI Proxy | http://localhost:8003 |
| Ingest | http://localhost:8001 |
| RabbitMQ 管理 | http://localhost:15672 (loghawk/<CHANGE_ME_PASSWORD>) |
| PostgreSQL | localhost:5432 (loghawk/<CHANGE_ME_PASSWORD>) |

**AI 功能启用（可选）：**
```bash
export OPENAI_API_KEY=sk-xxx
docker-compose up -d ai-proxy
```

---

## 方式 2: Kubernetes（生产级部署）

### 前置条件

1. 3 节点 K8s 集群（kubeadm 搭建）
2. CNI 网络插件（Flannel/Calico）
3. `kubectl` 可正常操作集群

### 节点规格

| 节点 | 角色 | 内存 | CPU |
|------|------|------|-----|
| k8s-master | control-plane | 4 GB | 2 核 |
| k8s-worker1 | worker | 2 GB | 1 核 |
| k8s-worker2 | worker | 2 GB | 1 核 |

### 快速部署

```bash
# 1. 确保节点就绪
kubectl get nodes

# 2. 安装 Go（编译需要）
snap install go --classic

# 3. 进入项目目录
cd ~/LogHawk

# 4. （可选）配置 AI API Key
export OPENAI_API_KEY=sk-xxx

# 5. 一键部署
bash scripts/deploy.sh
```

部署过程：
1. 编译 Go 服务 → 构建 Docker 镜像 → 导入 containerd
2. 分发镜像到 Worker 节点
3. 部署 PostgreSQL / RabbitMQ / Redis
4. 等待中间件就绪
5. 部署 Ingest / AI Proxy / Alerter / Chaos / Log Collector / Frontend
6. 部署 NetworkPolicy + ResourceQuota + Ingress
7. 部署 Prometheus / Grafana

### 部署后验证

```bash
# 检查 Pod 状态
kubectl -n loghawk get pods -o wide

# 检查服务
kubectl -n loghawk get svc

# 测试 Ingest
curl -X POST http://192.168.56.10:30080/api/ingest/health

# 打开前端
open http://192.168.56.10:30080
```

### 手动部署

如果自动脚本失败，可以分步手动部署：

```bash
# 1. 基础设施
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/configmap.yaml -f k8s/secret.yaml
kubectl apply -f k8s/postgres.yaml -f k8s/rabbitmq.yaml -f k8s/redis.yaml

# 2. 等待就绪
kubectl -n loghawk wait --for=condition=ready pod -l app=postgres --timeout=120s
kubectl -n loghawk wait --for=condition=ready pod -l app=rabbitmq --timeout=120s

# 3. 构建并导入镜像
cd services/ingest && CGO_ENABLED=0 go build -o ingest . && docker build -t loghawk/ingest:v1.0.0 . && docker save loghawk/ingest:v1.0.0 | ctr -n k8s.io images import -
cd ../ai-proxy && CGO_ENABLED=0 go build -o ai-proxy . && docker build -t loghawk/ai-proxy:v1.0.0 . && docker save loghawk/ai-proxy:v1.0.0 | ctr -n k8s.io images import -
cd ../frontend && docker build -t loghawk/frontend:v1.0.0 . && docker save loghawk/frontend:v1.0.0 | ctr -n k8s.io images import -

# 4. 部署业务
kubectl apply -f k8s/ingest.yaml
kubectl apply -f services/ai-proxy/deploy.yaml
kubectl apply -f k8s/frontend.yaml

# 5. 监控
kubectl apply -f k8s/prometheus.yaml -f k8s/grafana.yaml
```

---

## 方式 3: 编译运行（无 Docker）

```bash
# 编译
make build

# 运行 Ingest
./services/ingest/ingest &       # :8001

# 运行 AI Proxy
OPENAI_API_KEY=sk-xxx ./services/ai-proxy/ai-proxy &  # :8003

# 前端用任意 HTTP 服务器
cd services/frontend && python3 -m http.server 30080
```

---

## 服务端口映射

| 服务 | 集群内端口 | NodePort | 用途 |
|------|-----------|----------|------|
| Frontend | 80 | 30080 | 前端界面 |
| Ingest | 8001 | — | 日志摄入 |
| AI Proxy | 8003 | — | AI 分析 |
| PostgreSQL | 5432 | — | 数据库 |
| RabbitMQ | 5672 | — | 消息队列 |
| RabbitMQ 管理 | 15672 | — | 管理界面 |
| Redis | 6379 | — | 缓存 |
| Prometheus | 9090 | — | 指标采集 |
| Grafana | 3000 | 30300 | 仪表盘 |

---

## 卸载

```bash
# K8s
kubectl delete namespace loghawk

# Docker Compose
docker-compose down -v
```
