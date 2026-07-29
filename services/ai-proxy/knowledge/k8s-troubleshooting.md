# Kubernetes 排障速查

## 节点 NotReady

### 排查流程
```bash
kubectl get nodes
kubectl describe node <node-name> | grep -A5 Conditions
```

### 常见原因
- kubelet 挂了 → `systemctl status kubelet`
- 磁盘压力 → `df -h`
- 内存压力 → `free -h`
- CNI 网络插件异常 → `kubectl -n kube-system get pods | grep flannel`

## Service 无法访问

### 排查流程
```bash
kubectl get svc <svc> -o wide
kubectl get endpoints <svc>
kubectl describe svc <svc>
```

### 常见原因
- Endpoints 为空 → 检查 Pod selector 是否匹配
- Pod 未 Ready → 检查 readinessProbe
- 网络策略阻止 → `kubectl get networkpolicies`

## etcd 故障

### 症状
- API Server 无响应
- `etcd leader election failed`

### 修复步骤
```bash
# 检查 etcd 健康
ETCDCTL_API=3 etcdctl endpoint health

# 检查 etcd 状态
ETCDCTL_API=3 etcdctl endpoint status --write-out=table

# 碎片整理（谨慎操作）
ETCDCTL_API=3 etcdctl defrag
```
