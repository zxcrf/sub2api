#!/usr/bin/env bash
# 跑一个压测场景，结果追加到 results.jsonl。
# 用法: run.sh <label> <anthropic|responses|chat> <body.json> <并发> <时长> <上游首字节ms> <增量事件数> <事件间隔ms> [warmup] [ramp]
# 环境变量: BASE(网关地址) MOCK(mock 地址) KEYS_DIR(setup.sh 输出目录) LOADGEN(二进制) SERVER_PID(采样 CPU/RSS)
#          LOADGEN_CPUS(可选，用 taskset 把压测端绑到指定核，避免与被测进程抢 CPU)
set -euo pipefail
label=$1 proto=$2 body=$3 conc=$4 dur=$5 ttfb=$6 chunks=$7 interval=$8
BASE=${BASE:-http://127.0.0.1:8080}
MOCK=${MOCK:-http://127.0.0.1:9100}
KEYS_DIR=${KEYS_DIR:-.}
LOADGEN=${LOADGEN:-./loadgen}

case $proto in
  anthropic) path=/v1/messages;         auth=x-api-key; keys=$KEYS_DIR/keys-anthropic.txt ;;
  responses) path=/v1/responses;        auth=bearer;    keys=$KEYS_DIR/keys-openai.txt ;;
  chat)      path=/v1/chat/completions; auth=bearer;    keys=$KEYS_DIR/keys-openai.txt ;;
  *) echo "unknown proto $proto" >&2; exit 1 ;;
esac

curl -fsS "$MOCK/__cfg?ttfb_ms=$ttfb&chunks=$chunks&interval_ms=$interval" >/dev/null
${LOADGEN_CPUS:+taskset -c "$LOADGEN_CPUS"} "$LOADGEN" -url "$BASE$path" -auth "$auth" -keys-file "$keys" \
  -body "$body" -c "$conc" -d "$dur" -warmup "${9:-8s}" -ramp "${10:-5s}" \
  ${SERVER_PID:+-pid "$SERVER_PID"} -label "$label" | tee -a results.jsonl
