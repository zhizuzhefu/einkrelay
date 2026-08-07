# EInkRelay 接口用法示例

面向日常使用者的可执行示例集。契约的权威定义是 [openapi.yaml](openapi.yaml)；本文是「怎么调用」的实操手册，所有命令在 macOS / Linux 终端直接可跑。一个完整的真实场景（在 Kindle 上展示仪表盘，含 Markdown 路线的限制与图片路线的做法，均有真机画面）见 [dashboard-example.md](dashboard-example.md)。

## 0. 准备

```sh
# 设备地址：USBNetwork 通常是 192.168.15.244；Wi-Fi 用设备分到的地址
export KINDLE=192.168.15.244

# 取 token（只需一次；token 在设备上是 0600 文件，永不出现在任何响应里）
export TOKEN=$(ssh root@$KINDLE 'cat /var/local/einkrelay/token')
```

此后所有示例都引用 `$KINDLE` 与 `$TOKEN`。token 不要写进 shell 历史、脚本仓库或截图。

## 1. 查询状态

```sh
curl -s -H "Authorization: Bearer $TOKEN" http://$KINDLE:8080/v1/status
```

典型响应：

```json
{
  "version": "dev",
  "active": true,
  "mode": "active",
  "busy": false,
  "current": {"sha256": "…", "displayed_at": "2026-08-06T02:24:13Z"},
  "backend": {"name": "fbink", "state": "ready", "version": "FBInk 1.25.0 …"},
  "last_error": null
}
```

要点：`mode=active` 表示独占显示中；`busy=true` 表示正在刷屏，此时新的显示请求会得 `409 display_busy`；`current` 是最后成功画面的摘要与显示时间；`last_error` 是脱敏的最近错误。

## 2. 显示图片（PNG / JPEG）

```sh
# 完整包含（默认）：整图等比缩放到屏内，余白留边
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: image/png" --data-binary @page.png \
  "http://$KINDLE:8080/v1/display/image?fit=contain"

# 铺满裁切：等比放大到铺满整屏，超出部分裁掉
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: image/jpeg" --data-binary @photo.jpg \
  "http://$KINDLE:8080/v1/display/image?fit=cover"
```

约束：请求体上限 10 MiB；单边不超过 8192 像素、总像素不超过 3200 万；超限在分配大内存前就被 `413` / `422` 拒绝，屏幕不变。

## 3. 显示 Markdown

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: text/markdown; charset=utf-8" --data-binary @page.md \
  http://$KINDLE:8080/v1/display/markdown
```

支持标题、段落、粗体/斜体、列表、引用、代码块；未知或危险内容安全降级（不执行 HTML、脚本、远程资源）。注意两个排版现实：未启用 GFM 扩展，`|---|` 表格不会被渲染成表格；字体 manifest 只钉一个 face，`mono`/`bold`/`italic` 均回退到 regular（比例字体），因此代码块里的空格填充对齐与 `**粗体**` 在视觉上都打折扣。需要表格、图表或严格对齐时请改用自绘 PNG 推送，完整示例见 [dashboard-example.md](dashboard-example.md)。

### 调整字号

```sh
# font_size = 正文点数，12–72，缺省 18；标题、行距、边距等比缩放
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: text/markdown; charset=utf-8" --data-binary @page.md \
  "http://$KINDLE:8080/v1/display/markdown?font_size=32"
```

正文较长时用默认值；通知、单词卡等短内容建议 28–40。非法取值（非整数、越界、重复参数）返回 `400 invalid_parameter`，屏幕不变。

### 直接内联一小段（不写文件）

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: text/markdown" \
  --data-binary "# 提醒
**15:00** 开会" \
  "http://$KINDLE:8080/v1/display/markdown?font_size=36"
```

## 4. 退出独占模式

```sh
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://$KINDLE:8080/v1/system/exit
```

恢复是把进入独占前保存的那一帧原样写回面板，PW4 实测端到端约 **0.3 秒**，面板逐像素还原。与「任意角落 1 秒内连点 3 次」走同一条恢复路径；并发触发由 singleflight 合并为同一次执行，重复调用结果一致。退出后服务仍在运行，`/v1/status` 可查，重新进入需在设备上重新启动（KUAL 菜单项或 `/mnt/us/einkrelay/start.sh`）。

## 5. 错误码速查

所有错误都是 `{"error":{"code":"…","message":"…"}}`，不含 token 与路径：

| 状态码 | code | 含义与处置 |
| --- | --- | --- |
| 400 | `invalid_parameter` | 查询参数非法（未知参数、`font_size` 越界等） |
| 400 | `invalid_request` | 请求体为空 / exit 带了请求体 |
| 400 | `invalid_encoding` | Markdown 不是合法 UTF-8 |
| 401 | `unauthorized` | token 缺失或错误；先重新取 token |
| 404 | `not_found` | 路径不存在或方法不对（显示端点只接受 PUT） |
| 405 | `method_not_allowed` | `/v1/status`、`/v1/system/exit` 的方法错误 |
| 408 | `request_timeout` | 请求体读取超时 |
| 409 | `display_busy` | 正在刷屏，稍后重试 |
| 413 | `payload_too_large` | 请求体超限 |
| 415 | `unsupported_media_type` | Content-Type 或 charset 不对 |
| 422 | `decode_failed` / `render_failed` | 图片损坏或 Markdown 排版失败；屏幕不变 |
| 500 | `display_failed` / `persistence_failed` / `lifecycle_failed` / `internal_error` | 设备侧失败；查 `/v1/status` 与 Guardian 日志 |
| 504 | `transaction_timeout` | 显示事务超时 |

## 6. 一页速记

```sh
TOKEN=$(ssh root@192.168.15.244 'cat /var/local/einkrelay/token')
B=http://192.168.15.244:8080
AUTH="Authorization: Bearer $TOKEN"

curl -H "$AUTH" $B/v1/status                                    # 状态
curl -X PUT -H "$AUTH" -H 'Content-Type: image/png' \
  --data-binary @p.png "$B/v1/display/image?fit=contain"        # 图片
curl -X PUT -H "$AUTH" -H 'Content-Type: text/markdown' \
  --data-binary @p.md "$B/v1/display/markdown?font_size=32"     # Markdown
curl -X POST -H "$AUTH" $B/v1/system/exit                       # 退出独占
```
