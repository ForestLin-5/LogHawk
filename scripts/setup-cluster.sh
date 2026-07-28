#!/bin/bash
set -e
echo "🦅 LogHawk K8s 集群初始化"

# Master 节点
if [ "$(hostname)" = "master" ]; then
  echo ">>> Master 节点初始化"
  sudo kubeadm init --pod-network-cidr=10.244.0.0/16 --apiserver-advertise-address=192.168.56.10
  mkdir -p $HOME/.kube
  sudo cp /etc/kubernetes/admin.conf $HOME/.kube/config
  sudo chown $(id -u):$(id -g) $HOME/.kube/config
  kubectl apply -f https://raw.githubusercontent.com/flannel-io/flannel/master/Documentation/kube-flannel.yml
  echo ""
  echo ">>> 在 Worker 节点执行以下命令加入集群:"
  sudo kubeadm token create --print-join-command
else
  echo ">>> Worker 节点 — 请在 Master 上获取 join 命令后手动执行"
fi
