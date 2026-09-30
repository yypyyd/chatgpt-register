#!/bin/bash
# 批量采样 sentinel/req 响应里的 dx 程序（随机字体/字号/文字），用于建立文本盒子估算模型。
cd /opt/chatgpt-register
export SENTINEL_DEBUG_DUMP=/tmp/sdump2
rm -f /tmp/sdump2/sentinel_req.jsonl /tmp/sdump2.done
N=${1:-30}
for i in $(seq 1 "$N"); do
  ./gptlive5 -mode sentinel -timeout 2m >/dev/null 2>&1
done
echo done > /tmp/sdump2.done
