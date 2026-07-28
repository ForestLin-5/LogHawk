# 常见运维故障排查手册

## 1. 数据库连接池耗尽

### 症状
- 日志出现 `Connection pool exhausted` 或 `too many connections`
- 应用响应变慢或 5xx 错误
- `SHOW PROCESSLIST` 显示大量 Sleep 连接

### 根因
- 连接池配置过小（默认通常 100-200）
- 慢查询导致连接无法释放
- 应用未正确关闭连接（连接泄漏）

### 修复步骤
1. 排查慢查询：
```bash
SHOW FULL PROCESSLIST;
SELECT * FROM information_schema.processlist WHERE time > 10;
```
2. 临时扩容：
```sql
SET GLOBAL max_connections = 400;
```
3. 永久修改 my.cnf：
```ini
max_connections = 400
wait_timeout = 300
```
4. K8s 环境需同步修改 ConfigMap 并重启

---

## 2. 磁盘空间不足

### 症状
- 日志出现 `no space left on device` 或 `disk usage > 85%`
- Pod 被 Evicted
- 写入操作失败

### 修复步骤
1. 定位大文件：
```bash
du -sh /* 2>/dev/null | sort -rh | head -20
```
2. 清理 Docker/containerd：
```bash
crictl rmi --prune
```
3. 清理旧日志：
```bash
journalctl --vacuum-size=500M
find /var/log -name "*.log.*" -mtime +7 -delete
```

---

## 3. Pod CrashLoopBackOff

### 症状
- `kubectl get pods` 显示 CrashLoopBackOff
- 容器反复重启

### 修复步骤
1. 查看 Pod 日志：
```bash
kubectl logs <pod> --previous
kubectl describe pod <pod>
```
2. 常见原因：
- OOMKilled → 增加 memory limit
- ImagePullBackOff → 检查镜像地址和凭证
- 配置错误 → 检查 ConfigMap/Secret 挂载
- 端口冲突 → 检查 hostPort 是否重复

---

## 4. 证书即将过期

### 症状
- `certificate expiring in N days` 告警
- TLS 握手失败

### 修复步骤
```bash
# 检查证书过期时间
openssl x509 -in /path/to/cert.pem -noout -enddate

# K8s cert-manager 自动续期
kubectl get certificaterequests -A
kubectl describe certificate <name> -n <ns>
```
