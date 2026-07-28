# LogHawk 部署说明

## 环境要求

- Kubernetes 1.28+（已安装 nginx-ingress-controller）
- containerd / Docker + ctr
- Go 1.21+（用于构建）
- kubectl 已配置集群访问
- 至少 1 Master + 2 Worker（推荐）
- 节点总内存 >= 4Gi

## 部署前准备

### 1. 构建镜像

```bash
cd ~/LogHawk
bash scripts/deploy.sh
```

脚本会自动编译 Go 服务、构建 Docker 镜像、导入 containerd 并分发到 Worker 节点。

### 2. 配置敏感信息（必须）

**方式 A：命令行创建（推荐，密码不落地磁盘）**

```bash
kubectl -n loghawk create secret generic loghawk-secret \
  --from-literal=POSTGRES_USER=loghawk \
  --from-literal=POSTGRES_PASSWORD=$(openssl rand -hex 16) \
  --from-literal=RABBITMQ_USER=admin \
  --from-literal=RABBITMQ_PASS=$(openssl rand -hex 16) \
  --from-literal=OPENAI_API_KEY=sk-your-real-key \
  --from-literal=CHAOS_API_TOKEN=$(openssl rand -hex 32)

kubectl -n loghawk create secret docker-registry aliyun-registry \
  --docker-server=crpi-kwcxn84zbm6expkn.cn-guangzhou.personal.cr.aliyuncs.com \
  --docker-username=your-user \
  --docker-password=your-pass
```

**方式 B：修改 YAML 后 apply（仅测试环境）**

编辑 k8s/secret.yaml 和 k8s/registry-secret.yaml，替换占位符后执行：

```bash
kubectl apply -f k8s/secret.yaml
kubectl apply -f k8s/registry-secret.yaml
```

### 3. 配置 AI Proxy（可选）

如果不需要 AI 功能，可以跳过。需要时创建：

```bash
kubectl -n loghawk create secret generic ai-proxy-secret \
  --from-literal=OPENAI_API_KEY=sk-your-key \
  --from-literal=OPENAI_BASE_URL=https://api.openai.com/v1 \
  --from-literal=OPENAI_MODEL=gpt-4o-mini
```

### 4. 修改 Ingress 域名（必须）

编辑 k8s/ingress.yaml，将 loghawk.local 替换为实际域名：

```yaml
rules:
  - host: loghawk.local  # 改为你自己的域名
```

如没有域名，可通过 NodePort 访问：http://<MasterIP>:30080

## 一键部署

```bash
bash scripts/deploy.sh
```

部署过程：
1. 编译 Go 服务 -> 构建 Docker 镜像 -> 导入 containerd
2. 分发镜像到 Worker 节点
3. 部署 PostgreSQL / RabbitMQ / Redis
4. 等待中间件就绪
5. 部署 Ingest / AI Proxy / Alerter / Chaos / Log Collector / Frontend
6. 部署 NetworkPolicy + ResourceQuota + Ingress
7. 部署 Prometheus / Grafana

## 部署验证

```bash
# 查看 Pod 状态（全部 Running 即正常）
kubectl -n loghawk get pods -o wide

# 查看服务
kubectl -n loghawk get svc

# 查看 Ingress
kubectl -n loghawk get ingress

# 测试摄入接口
curl -X POST http://192.168.56.10:30080/api/ingest/health
```

## 访问方式

| 入口 | 地址 | 说明 |
|------|------|------|
| 前端 | http://<MasterIP>:30080 或域名 | 通过 Ingress 或 NodePort |
| Grafana | http://<MasterIP>:30300 | 账号 admin / 密码见配置 |
| AI Proxy | 由前端 Nginx 反向代理 | 不单独暴露 |

AI Proxy 不通过 Ingress 直接暴露。前端在 K8s 内网中通过相对路径 /api/* 访问。

## 故障排查

| 现象 | 排查命令 |
|------|----------|
| Pod 拉镜像失败 | kubectl -n loghawk describe pod <pod-name> |
| Pod 权限不足 | kubectl -n loghawk logs <pod-name> |
| 前端 404 | 确认 Ingress 域名解析，或改用 NodePort |
| AI 功能不可用 | 检查 ai-proxy-secret 是否存在 |
| Chaos 401 | 确认 CHAOS_API_TOKEN 已配置 |

## 回滚

```bash
# 查看历史版本
kubectl -n loghawk rollout history deployment/ingest

# 回滚到上一版本
kubectl -n loghawk rollout undo deployment/ingest

# 回滚到指定版本
kubectl -n loghawk rollout undo deployment/ingest --to-revision=1
```

## 版本发布

```bash
# 构建并部署指定版本
IMAGE_TAG=v1.1.0 bash scripts/deploy.sh
```

## 卸载

```bash
kubectl delete namespace loghawk
```

## 重要安全提示

1. **敏感信息**：生产环境必须使用 kubectl create secret 命令行创建 Secret，避免将真实密码提交到 Git。
2. **Chaos 认证**：所有 Chaos 故障演练接口都需要 Bearer Token。
3. **镜像标签**：部署脚本默认使用 v1.0.0 标签，生产环境应显式指定 IMAGE_TAG。
4. **Ingress TLS**：生产环境建议为 Ingress 配置 TLS 证书，当前 YAML 未包含 TLS。