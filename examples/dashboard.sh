#!/bin/sh
# 用纯 curl 把一块仪表盘推到 Kindle。没有任何额外依赖。
set -eu
: "${EINKRELAY_HOST:?set EINKRELAY_HOST, e.g. 192.168.15.244:8080}"
: "${EINKRELAY_TOKEN:?set EINKRELAY_TOKEN}"

# 列对齐靠代码块（等宽），不要用 |---| 表格：服务端是 CommonMark 核心，
# 没开 GFM 扩展，表格语法会原样显示成竖线。
row() { printf '%-14s %10s   %s\n' "$1" "$2" "$3"; }

body=$(cat <<EOF
# 服务概览

**$(date '+%Y-%m-%d %H:%M')**  ·  生产环境

## 核心指标

~~~
$(row 指标 当前值 趋势)
$(row -------------- ---------- ------)
$(row QPS 1284 "▲ +6%")
$(row P99延迟 218ms "▼ -12ms")
$(row 错误率 0.03% "— 持平")
$(row 在线实例 12/12 "— 正常")
~~~

## 待办

- 结算对账任务 **积压 3 批**
- 证书 \`api.example.com\` 还有 **11 天**到期

> 上次全量巡检：今天 09:14，无异常。
EOF
)

printf '%s' "$body" | curl -sS --fail-with-body \
  -X PUT "http://$EINKRELAY_HOST/v1/display/markdown?font_size=26" \
  -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  -H 'Content-Type: text/markdown; charset=utf-8' \
  --data-binary @- -o /dev/null -w '推送完成 HTTP %{http_code}，耗时 %{time_total}s\n'
