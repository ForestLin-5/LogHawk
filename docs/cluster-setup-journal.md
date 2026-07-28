# 🛠️ kubeadm 3 节点集群搭建实战日志

> 时间：2026年7月27日 12:00–16:00
> 操作人：林浩森（大嘉豪）
> 环境：VMware Workstation Pro + Ubuntu 24.04 Server ×3

---

## 架构

```
┌─────────────────┐
│  Master (.10)    │  4096MB / 2vCPU / 30G
│  control-plane   │
└────────┬────────┘
         │
    ┌────┴────┐
    ▼         ▼
┌─────────┐ ┌─────────┐
│Worker1  │ │Worker2  │  2048MB / 1vCPU / 25G
│(.11)    │ │(.12)    │
└─────────┘ └─────────┘
```

---

## 时间线

| 时间 | 阶段 |
|---|---|
| 12:00 | 从原始 Ubuntu 24.04 VM 克隆 ×3 |
| 12:30 | 配静态 IP — 踩坑 MAC 重复、netplan 不生效 |
| 13:00 | `ip addr flush` 硬设 IP，三台互通 |
| 13:15 | 发现 VM 内存全 8G → 降到 4G/2G/2G |
| 13:30 | 三台跑 `init.sh`（containerd + kubeadm） |
| 14:00 | Master `kubeadm init` 失败 — K3s 残留、pause 被墙 |
| 15:00 | pause 3.9 伪装 3.10.1、Flannel ghcr.io 龟速拉取 |
| 15:22 | Master `kubeadm init` 成功 |
| 15:30 | Flannel 镜像拉取成功，Master Ready |
| 16:00 | Worker1、Worker2 加入，三台 Ready |

---

## 🔥 故障 1：MAC 地址重复 → DHCP 分配相同 IP

### 现象
三台 VM 克隆后全部获取 `192.168.234.128`（后变 130、132）。

### 根因
VMware 克隆时默认保留原始 MAC 地址。三台 MAC 相同 → DHCP 服务器认为是一台机器。

### 排查
```bash
hostname -I
# 三台都输出 192.168.234.128
```

### 解决
VMware 设置 → 网络适配器 → 高级 → 生成新 MAC 地址。然后每台配静态 IP 彻底告别 DHCP。

---

## 🔥 故障 2：netplan 静态 IP 不生效

### 现象
```yaml
# 写了但无效
dhcp4: false
addresses: [192.168.234.10/24]
```
`hostname -I` 仍然显示旧的 DHCP IP。

### 根因
Ubuntu 24.04 默认用 NetworkManager 渲染 netplan。写 `dhcp4: false` 不生效，需要 `renderer: networkd` + `dhcp4: no`。

### 最终方案
放弃 netplan，直接用 `ip` 命令硬设：
```bash
sudo ip addr flush dev ens33
sudo ip addr add 192.168.234.10/24 dev ens33
sudo ip route add default via 192.168.234.2
```

**教训**：netplan 有两套渲染器（NetworkManager / networkd），语法不同。生产环境优先用 `/etc/network/interfaces` 或 `ip` 命令硬设。

---

## 🔥 故障 3：K3s 残留进程拒绝死亡

### 现象
`kubeadm init` 报错 `container runtime is not running`，日志显示还在操作 `/var/lib/rancher/k3s/data/`。

### 根因
克隆源 VM 上装过 K3s。`k3s` 服务开机自动启动，网络虚拟网卡（flannel、cni0、docker0）删了又重建。

### 排查
```bash
ps aux | grep k3s          # 发现 k3s 进程
ls /var/lib/rancher        # 数据目录还在
which kubectl              # /usr/local/bin/kubectl → k3s
```

### 解决
```bash
sudo k3s-killall.sh
sudo pkill -9 -f k3s
sudo systemctl stop k3s && sudo systemctl disable k3s
sudo rm -rf /var/lib/rancher /etc/rancher /var/lib/kubelet
sudo rm -f /usr/local/bin/kubectl /usr/local/bin/crictl
```

**教训**：K3s 会在系统中埋大量钩子。从 K3s 迁移到 kubeadm 不是"卸载就行"，是"追杀每一个残留进程、文件和符号链接"。

---

## 🔥 故障 4：`kubeadm init` 超时 — pause 镜像被墙

### 现象
```
[wait-control-plane] timed out waiting for the condition
```
containerd 日志：
```
failed to pull image "registry.k8s.io/pause:3.10.1"
dial tcp 142.251.188.82:443: connect: connection refused
```

### 根因
`--image-repository registry.aliyuncs.com/google_containers` 只代理了 API Server、etcd、Controller Manager、Scheduler 四大件。**pause 容器仍然从 Google 直拉**——而 Google 被墙。

### 排查
```bash
sudo journalctl -xeu containerd | grep pause
# failed to do request: Head "https://europe-west3-docker.pkg.dev/..."
```

### 解决
pause 镜像本质是一个什么都不做的空容器，版本号差异无影响：
```bash
sudo ctr -n k8s.io images pull registry.aliyuncs.com/google_containers/pause:3.9
sudo ctr -n k8s.io images tag registry.aliyuncs.com/google_containers/pause:3.9 registry.k8s.io/pause:3.10.1
```

**教训**：K8s 的核心组件有 imageRepository 参数可换源；**pause 是例外**，必须手动导入或配 containerd mirror。

---

## 🔥 故障 5：Flannel ghcr.io 龟速拉取

### 现象
```
Pulling from OCI Registry (ghcr.io/flannel-io/flannel:v0.28.8)
elapsed: 436.6s total: 20.5 Mi (48.0 KiB/s)
```
31MB 镜像拉了 7 分钟。

### 根因
ghcr.io（GitHub Container Registry）在国内无 CDN，单线程几十 KB/s。

### 解决
等。没有更好的办法。ghcr.io 在中国大陆没有镜像代理。

### 优化方向（未来）
- 预拉通用 CNI 镜像到本地 Harbor/ACR
- 提前 `kubeadm config images pull` + `docker save` + SCP 分发
- 考虑换 Calico（阿里云有代理）

---

## 🔥 故障 6：K3s 绑架 kubectl

### 现象
```bash
kubectl get nodes
# INFO[0000] Acquiring lock file /var/lib/rancher/k3s/data/.lock
```

### 根因
K3s 把 `/usr/local/bin/kubectl` 做成了指向 K3s 二进制的软链接：
```bash
ls -la /usr/local/bin/kubectl
# kubectl -> k3s
```

### 解决
```bash
sudo rm -f /usr/local/bin/kubectl
/usr/bin/kubectl get nodes   # 正常
```

---

## 🔥 故障 7：dpkg 内核升级中断

### 现象
`init.sh` 在 `apt update` 时报错：
```
E: dpkg was interrupted, you must manually run 'sudo dpkg --configure -a'
```

### 根因
`apt install containerd` 时自动拉取新内核 6.8.0-136，安装到一半路径中断。

### 解决
```bash
sudo dpkg --configure -a
```

**教训**：生产环境装软件务必加 `--no-upgrade`，防止内核被顺带升级导致重启后失联。

---

## ✅ 最终状态

```bash
$ kubectl get nodes
NAME          STATUS   ROLES           AGE     VERSION
k8s-master    Ready    control-plane   22m     v1.28.2
k8s-worker1   Ready    <none>          42s     v1.28.2
k8s-worker2   Ready    <none>          4m17s   v1.28.2
```

---

## 📊 排障耗时统计

| 故障 | 耗时 | 占比 |
|---|---|---|
| MAC 重复 + netplan 不生效 | 45 min | 21% |
| K3s 残留追杀 | 30 min | 14% |
| pause 被墙 | 40 min | 19% |
| Flannel 龟速拉取 | 30 min | 14% |
| dpkg 内核中断 | 10 min | 5% |
| K3s 绑架 kubectl | 5 min | 2% |
| **真正在装软件的时间** | **55 min** | **26%** |

**结论**：搭集群 74% 的时间在排障，26% 的时间在装软件。这就是运维。

---

## 🧠 面试话术

> "我在本地 VMware 上用 kubeadm 搭过 3 节点 K8s 集群。过程中遇到过 MAC 地址重复导致 DHCP 分配相同 IP、K3s 残留进程拒绝停止、Google pause 镜像被墙需要手动伪装版本、ghcr.io 在国内无 CDN 拉取极慢等问题——每个都独立排查并解决了。最终三台节点全部 Ready，现在可以随时在上面部署微服务。"

---

> 💡 本日志为 LogHawk 项目集群搭建阶段的完整排障记录。每条故障均可独立复现、独立讲解。
