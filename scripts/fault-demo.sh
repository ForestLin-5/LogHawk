#!/bin/bash
set -e

SCENARIO=${1:-"list"}
NS="loghawk"

case $SCENARIO in
  1|"node-down")
    echo "🔥 场景1: 工作节点宕机 — 停止 Worker1"
    echo "   vagrant halt worker1"
    echo "   观察: Ingest 副本漂移到 Worker2, 摄入速率 1 秒恢复, 0 数据丢失"
    ;;
  2|"queue-pile")
    echo "🔥 场景2: 队列堆积自愈"
    echo "   kubectl -n $NS scale deploy analyzer --replicas=0"
    echo "   sleep 60"
    echo "   kubectl -n $NS scale deploy analyzer --replicas=2"
    echo "   观察: Prometheus 队列深度图拉满 -> 30 秒消化完毕"
    ;;
  3|"ws-reconnect")
    echo "🔥 场景3: WebSocket 断线重连"
    echo "   kubectl -n $NS delete pod -l app=alerter"
    echo "   观察: 前端红框'连接断开' -> 3秒自动重连"
    ;;
  4|"disk-pressure")
    echo "🔥 场景4: 磁盘压力调度"
    echo "   在 Worker2 上: dd if=/dev/zero of=/tmp/bigfile bs=1M count=5000"
    echo "   观察: disk-pressure 污点出现 -> 新 Pod 调度到 Worker1"
    ;;
  5|"rolling-update")
    echo "🔥 场景5: 滚动更新零停机"
    echo "   kubectl -n $NS set image deploy/ingest ingest=loghawk/ingest:v2"
    echo "   kubectl -n $NS rollout status deploy/ingest"
    echo "   观察: 旧 Pod -> 新 Pod 逐个替换, 摄入速率无显著波动"
    ;;
  all)
    echo "🔥 全部故障场景演示"
    bash $0 1; echo ""; sleep 3
    bash $0 2; echo ""; sleep 3
    bash $0 3; echo ""; sleep 3
    bash $0 5;
    ;;
  list|*)
    echo "🦅 LogHawk 故障演示场景:"
    echo "  1 / node-down      工作节点宕机"
    echo "  2 / queue-pile      队列堆积自愈"
    echo "  3 / ws-reconnect    WebSocket 断线重连"
    echo "  4 / disk-pressure   磁盘压力调度"
    echo "  5 / rolling-update  滚动更新零停机"
    echo "  all                 全部演示"
    echo ""
    echo "用法: bash scripts/fault-demo.sh <场景编号>"
    ;;
esac
