# LogHawk K8s 部署指南

## 前提条件

- 3 节点 K8s 集群（kubeadm），节点就绪
- Master 节点已安装 `go` (1.21+)、`docker` 或 `nerdctl`、`ctr` (containerd)
- 所有节点使用 containerd 作为容器运行时
- `kubectl` 可正常操作集群

```bash
# 确认节点全部 Ready
kubectl get nodes
# NAME          STATUS   ROLES           AGE   VERSION
# k8s-master    Ready    control-plane   36m   v1.28.2
# k8s-worker1   Ready    <none>          14m   v1.28.2
# k8s-worker2   Ready    <none>          18m   v1.28.2
```

---

## 部署概览

部署后会启动以下 Pod：

| Pod | 类型 | 副本 | 镜像 | 来源 |
|-----|------|------|------|------|
| ingest | Deployment | 2 | `loghawk/ingest:v1.0.0` | 本地构建 |
| ai-proxy | Deployment | 1 | `loghawk/ai-proxy:v1.0.0` | 本地构建 |
| alerter | Deployment | 1 | `loghawk/alerter:v1.0.0` | 本地构建 |
| chaos | Deployment | 1 | `loghawk/chaos:v1.0.0` | 本地构建 |
| frontend | Deployment | 1 | `loghawk/frontend:v1.0.0` | 本地构建 |
| log-collector | **DaemonSet** | 3 | `loghawk/log-collector:v1.0.0` | 本地构建 |
| postgres | StatefulSet | 1 | `postgres:15-alpine` | Docker Hub |
| rabbitmq | Deployment | 1 | `rabbitmq:3.12-alpine` | Docker Hub |
| redis | Deployment | 1 | `redis:7-alpine` | Docker Hub |
| prometheus | Deployment | 1 | `prom/prometheus:v2.51.0` | Docker Hub |
| grafana | Deployment | 1 | `grafana/grafana:10.4.0` | Docker Hub |

总计：14 个 Pod，~800Mi 内存请求

---

## 方式 1: 一键部署脚本（推荐）

```bash
# 1. 拷贝项目到 Master
cd ~/LogHawk

# 2. 安装 Go（如果没有）
snap install go --classic

# 3. （可选）设置 AI API Key
export OPENAI_API_KEY=sk-xxx

# 4. 一键部署
bash scripts/deploy.sh
```

脚本会自动：
- 编译 5 个 Go 服务 → 构建 Docker 镜像 → 导入 containerd
- 分发镜像到 Worker 节点（可选，失败不影响）
- 创建 Namespace + PriorityClass + ConfigMap + Secret
- 部署中间件 → 等待就绪 → 部署业务服务
- 部署 NetworkPolicy + ResourceQuota + Ingress
- 部署 Prometheus + Grafana

---

## 方式 2: Helm 部署

```bash
# 安装
helm install loghawk ./charts/loghawk \
  -n loghawk --create-namespace \
  --set aiProxy.env.openaiApiKey=sk-xxx

# 查看状态
helm list -n loghawk
kubectl -n loghawk get pods -o wide

# 升级配置
helm upgrade loghawk ./charts/loghawk -n loghawk \
  --set frontend.nodePort=30080

# 卸载
helm uninstall loghawk -n loghawk
```

> ⚠️ Helm 部署前仍需手动构建 5 个 Go 服务镜像并导入 containerd。

---

## 方式 3: 手动分步部署

### 第 1 步：构建镜像

```bash
cd ~/LogHawk

# 构建每个 Go 服务
for svc in ingest ai-proxy alerter log-collector chaos; do
  echo "🔨 Building $svc..."
  cd services/$svc
  CGO_ENABLED=0 go build -ldflags="-s -w" -o $svc .
  docker build -t loghawk/$svc:v1.0.0 . || nerdctl build -t loghawk/$svc:v1.0.0 .
  docker save loghawk/$svc:v1.0.0 | ctr -n k8s.io images import -
  cd ../..
done

# 构建前端
cd services/frontend
docker build -t loghawk/frontend:v1.0.0 .
docker save loghawk/frontend:v1.0.0 | ctr -n k8s.io images import -
cd ../..

# 验证镜像已导入
crictl images | grep loghawk
```

### 第 2 步：部署基础设施

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/priorityclass.yaml
kubectl apply -f k8s/configmap.yaml -f k8s/secret.yaml
kubectl apply -f k8s/postgres.yaml -f k8s/rabbitmq.yaml -f k8s/redis.yaml

# 等待中间件就绪
kubectl -n loghawk wait --for=condition=ready pod -l app=postgres --timeout=120s
kubectl -n loghawk wait --for=condition=ready pod -l app=rabbitmq --timeout=120s
kubectl -n loghawk wait --for=condition=ready pod -l app=redis --timeout=60s
```

### 第 3 步：部署业务服务

```bash
kubectl apply -f k8s/ingest.yaml
kubectl apply -f services/ai-proxy/deploy.yaml
kubectl apply -f k8s/alerter.yaml
kubectl apply -f k8s/frontend.yaml
kubectl apply -f k8s/log-collector.yaml
kubectl apply -f k8s/chaos.yaml
```

### 第 4 步：部署监控

```bash
kubectl apply -f k8s/prometheus.yaml
kubectl apply -f k8s/grafana.yaml
kubectl apply -f k8s/resourcequota.yaml
```

### 第 5 步：验证

```bash
# 查看所有 Pod
kubectl -n loghawk get pods -o wide

# 查看服务
kubectl -n loghawk get svc

# 测试 Ingest
curl -X POST http://192.168.56.10:30080/api/ingest/health

# 测试 AI Proxy
curl http://192.168.56.10:30080/api/health

# 打开前端
# http://192.168.56.10:30080
```

---

## 部署后验证清单

```bash
# ✅ 所有 Pod Running
kubectl -n loghawk get pods | grep -v Running

# ✅ Ingest 健康
curl -s http://192.168.56.10:30080/api/ingest/health

# ✅ Ingest 指标
curl -s http://192.168.56.10:30080/api/ingest/metrics | head -5

# ✅ AI Proxy 健康
curl -s http://192.168.56.10:30080/api/health

# ✅ Alerter WebSocket
curl -s http://192.168.56.10:30080/api/alerter/health

# ✅ 前端可访问
curl -s http://192.168.56.10:30080 | head -5

# ✅ Grafana 可访问
curl -s http://192.168.56.10:30300

# ✅ DaemonSet 每节点都有 Pod
kubectl -n loghawk get pods -l app=log-collector -o wide

# ✅ HPA 生效
kubectl -n loghawk get hpa
```

---

## 配置 AI 功能

### 使用 OpenAI

```bash
kubectl -n loghawk create secret generic ai-proxy-secret \
  --from-literal=OPENAI_API_KEY=sk-xxx \
  --from-literal=OPENAI_BASE_URL=https://api.openai.com/v1 \
  --from-literal=OPENAI_MODEL=gpt-4o-mini \
  --dry-run=client -o yaml | kubectl apply -f -

# 重启 AI Proxy 使 Secret 生效
kubectl -n loghawk rollout restart deploy ai-proxy
```

### 使用 Ollama（本地免费）

```bash
# 在 Master 节点安装 Ollama
curl -fsSL https://ollama.com/install.sh | sh
ollama pull qwen2.5:7b

# 配置 AI Proxy 使用 Ollama
kubectl -n loghawk create secret generic ai-proxy-secret \
  --from-literal=OPENAI_API_KEY=ollama \
  --from-literal=OPENAI_BASE_URL=http://localhost:11434/v1 \
  --from-literal=OPENAI_MODEL=qwen2.5:7b \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n loghawk rollout restart deploy ai-proxy
```

---

## 使用 Chaos 故障演练

Chaos 服务所有接口都需要 Bearer Token 认证（Token 来自 `loghawk-secret` 的 `CHAOS_API_TOKEN`）：

```bash
export CHAOS_TOKEN="<your-chaos-api-token>"

# 查看场景列表
curl -H "Authorization: Bearer $CHAOS_TOKEN" http://192.168.56.10:30080/api/chaos/status

# 搞砸场景 1（Pod Crash）
curl -H "Authorization: Bearer $CHAOS_TOKEN" -X POST "http://192.168.56.10:30080/api/chaos/break?scenario=pod-deleted"

# 获取提示
curl -H "Authorization: Bearer $CHAOS_TOKEN" "http://192.168.56.10:30080/api/chaos/hint?scenario=pod-deleted&level=1"

# 修复后在页面上点 ✅ 验证，或用命令：
curl -H "Authorization: Bearer $CHAOS_TOKEN" -X POST "http://192.168.56.10:30080/api/chaos/verify?scenario=pod-deleted"

# 重置全部
curl -H "Authorization: Bearer $CHAOS_TOKEN" -X POST http://192.168.56.10:30080/api/chaos/reset
```

---

## 访问地址

| 服务 | 地址 |
|------|------|
| 前端 | `http://192.168.56.10:30080` |
| Grafana | `http://192.168.56.10:30300` (admin/<CHANGE_ME_GRAFANA_PASSWORD>) |
| AI Proxy | `http://192.168.56.10:30080/api/` (Nginx 反向代理) |

---

## 卸载

```bash
# 脚本部署的
kubectl delete namespace loghawk

# Helm 部署的
helm uninstall loghawk -n loghawk
kubectl delete namespace loghawk
```

---

## 常见问题

### Q: Pod 一直 ImagePullBackOff

```bash
# 检查镜像是否已导入 containerd
crictl images | grep loghawk

# 如果缺少，手动导入
docker save loghawk/ingest:v1.0.0 | ctr -n k8s.io images import -
```

### Q: Ingest 连不上 RabbitMQ

```bash
# 检查 ConfigMap
kubectl -n loghawk get configmap loghawk-config -o yaml | grep RABBITMQ

# 确认 RabbitMQ Pod 已 Running
kubectl -n loghawk get pods -l app=rabbitmq
```

### Q: AI Proxy 启动但分析失败

```bash
# 检查 Secret
kubectl -n loghawk get secret ai-proxy-secret -o yaml

# 查看日志
kubectl -n loghawk logs -l app=ai-proxy | tail -20
```

### Q: 前端打开但数据为空

检查 Nginx 反向代理是否正确转发 `/api/` 请求到 AI Proxy。
