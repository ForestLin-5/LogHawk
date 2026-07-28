# LogHawk 故障场景演示手册

## 概述

LogHawk 项目的一大亮点是可以在面试现场演示 **故障自愈**。以下 5 个场景覆盖了常见的运维故障，每个场景都可独立演示。

---

## 场景 1: 工作节点宕机

**演示目标：** 展示 Pod 自动漂移，零数据丢失

```bash
# 查看当前 Pod 分布
kubectl -n loghawk get pods -o wide

# 停止 Worker1
vagrant halt k8s-worker1

# 观察变化（每 2 秒刷新）
watch -n 2 'kubectl -n loghawk get pods -o wide'

# 恢复
vagrant up k8s-worker1
```

**预期结果：**
- Worker1 上的 Pod 进入 Terminating
- 30 秒内相同副本在 Worker2/Master 上 Running
- RabbitMQ 队列消息持久化，不丢失
- 前端 WebSocket 短暂中断后自动重连

---

## 场景 2: 日志摄入中断自愈

**演示目标：** 展示 Ingest 服务缩容后日志摄入中断，扩容后自动恢复

```bash
# 停止 Ingest（模拟故障）
kubectl -n loghawk scale deploy ingest --replicas=0

# 尝试推送日志（会失败或超时）
for i in {1..20}; do
  curl -X POST http://192.168.56.10:30080/api/ingest/ingest \
    -H "Content-Type: application/json" \
    -d "[{\"timestamp\":\"$(date +%T)\",\"level\":\"ERROR\",\"message\":\"test $i\"}]"
done

# 观察前端日志流中断

# 恢复
kubectl -n loghawk scale deploy ingest --replicas=2

# 观察 10 秒内日志流恢复，堆积的日志被处理
```

---

## 场景 3: WebSocket 断线重连

**演示目标：** 前端断线 3 秒自动重连

```bash
# 删除 Alerter Pod
kubectl -n loghawk delete pod -l app=alerter

# 观察前端：
# 1. 右上角 WebSocket 状态变 🔴 "连接断开"
# 2. 3 秒内自动变回 🟢 "WebSocket 在线"
# 3. 断线期间的告警在重连后补齐
```

---

## 场景 4: 磁盘压力调度

**演示目标：** K8s 污点驱逐 + Pod 重调度

```bash
# SSH 到 Worker2
vagrant ssh k8s-worker2

# 模拟磁盘压力
sudo dd if=/dev/zero of=/tmp/bigfile bs=1M count=5000
# 等待 K8s 检测到磁盘压力（约 1 分钟）

# 在 Master 上观察
kubectl describe node k8s-worker2 | grep Taints
# 预期看到: node.kubernetes.io/disk-pressure

kubectl -n loghawk get pods -o wide
# Worker2 上的 Pod 被驱逐，重新调度到 Worker1/Master

# 清理
sudo rm /tmp/bigfile
```

---

## 场景 5: 滚动更新零停机

**演示目标：** 更新期间 WebSocket 不断，摄入速率无波动

```bash
# 1. 打开 Grafana 仪表盘 → 摄入速率面板

# 2. 触发滚动更新
kubectl -n loghawk set image deploy/ingest ingest=loghawk/ingest:v1.0.0
kubectl -n loghawk rollout status deploy/ingest

# 3. 观察：
#    - Grafana 摄入速率曲线仅微小波动（< 10%）
#    - 前端 WebSocket 未断开
#    - 旧 Pod 优雅终止 → 新 Pod 接收流量
```

---

## 场景 6: 告警链路验证

**演示目标：** 展示从日志注入到前端告警面板的全链路

```bash
# 1. 注入 CRIT 级别日志（触发"立即告警"规则）
kubectl -n loghawk exec deploy/ingest -- wget -qO- --post-data='[{"timestamp":"2026-07-28T16:22:00Z","level":"CRIT","service":"payment","message":"CRITICAL: 数据库连接池耗尽"}]' --header='Content-Type: application/json' http://localhost:8001/ingest

# 2. 等待 3 秒让 alerter 消费处理
sleep 3

# 3. 查看 alembicr 生成的告警
kubectl -n loghawk exec deploy/alerter -- wget -qO- http://localhost:8004/alerts

# 4. 刷新前端告警中心页面查看
```

**预期结果：**
- 前端告警中心出现 "CRITICAL: 1 条 CRIT 级别日志" 告警卡片
- 告警统计数字更新
- 浏览器控制台无错误

**告警规则一览：**
| 规则 | 条件 | 窗口 | 冷却 |
|------|------|------|------|
| CRIT 立即告警 | CRIT ≥1 | 1 分钟 | 30s |
| ERROR 突发 | ERROR >5 | 1 分钟 | 60s |
| 服务持续报错 | 单服务 ERROR >10 | 2 分钟 | 120s |

---

## 面试话术

| 场景 | 面试能讲的 |
|------|-----------|
| 节点宕机 | "Pod 自动漂移，消息队列持久化保证 0 数据丢失" |
| 日志摄入中断 | "Ingest 服务缩容导致日志摄入中断，扩容后 10 秒内自动恢复" |
| 断线重连 | "WebSocket 心跳检测，3 秒自动重连，断线期间告警不丢失" |
| 磁盘压力 | "K8s 污点驱逐机制，Pod 自动重调度到健康节点" |
| 滚动更新 | "Readiness Probe + 优雅终止，更新期间摄入速率无显著波动" |
