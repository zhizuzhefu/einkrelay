# EInkRelay

EInkRelay 是 Kindle Paperwhite 4（固件 5.18.1.1.1、ARMv7）上的本地 REST 全屏显示服务。它是纯 Go、`CGO_ENABLED=0` 的单一可执行文件：`guard` 与 `serve` 分别运行在独立进程中，前者监督后者、处理触摸角落退出和原生 UI 恢复。本项目不依赖 KUAL，也不需要 Mac、树莓派或云端常驻程序；USBNetwork 与 Wi-Fi 均访问同一监听器和同一版本化 API。

许可证为 [Apache-2.0](LICENSE)。完整机器可读接口见 [docs/openapi.yaml](docs/openapi.yaml)，日常调用的可执行示例见 [docs/api-examples.md](docs/api-examples.md)，真实场景示例（把本机 Claude / Codex / Grok / Kimi / GLM / MiniMax / Antigravity / DeepSeek / 百炼的额度使用情况做成仪表盘推到 Kindle，[examples/quota-dashboard/](examples/quota-dashboard/) 是可运行的纯 Go 程序）见 [docs/dashboard-example.md](docs/dashboard-example.md)，运行参数见 [docs/configuration.md](docs/configuration.md)，第三方声明见 [docs/third-party.md](docs/third-party.md)。

可复现 release 构建见 [docs/release-build.md](docs/release-build.md)。

> 状态：仓库中的单元、合同、渲染、持久化、Guardian/手势测试和 Linux ARMv7 交叉构建是本地证据，不是 Kindle 硬件验收。**硬件验收状态为「未执行」**：下文“PW4 硬件验收”是验收清单，尚未在任何真实设备上逐项执行。

## API 概览

默认服务地址为 `0.0.0.0:8080`。所有 `/v1/*` 路径必须带且只能带一个 `Authorization: Bearer <token>`；没有匿名端点，未知路径（含已移除的 `/healthz`）一律返回 `404 not_found`。认证在路由、方法检查、事务锁、参数和请求体处理之前完成（`cmd/eink-relay/main.go:567`），因此未授权请求不会渲染、调用 FBInk 或变更持久状态。服务只开**一个**监听器：单次 `ListenAndServe` 绑定 `EINKRELAY_LISTEN_ADDRESS:EINKRELAY_LISTEN_PORT`（默认 `0.0.0.0:8080`，`main.go:219-221`、`main.go:1116-1118`），USBNetwork 与 Wi-Fi 只是通往同一进程同一端口的两条链路，两侧看到的是完全相同的端点、鉴权、限额与状态。主机侧不需要任何常驻程序、代理或转发器；token 只由 `serve` 从 `0600` 的本地文件加载（`main.go:1059`），从不出现在命令行、日志、`/v1/status` 或错误响应里。

| 方法与路径 | 请求 | 成功结果 |
| --- | --- | --- |
| `GET /v1/status` | Bearer token | 版本、`active`/`inactive` 模式、忙闲、**可见面板几何**、当前画面摘要与时间、FBInk 状态、脱敏的最近错误；快照不合 schema 时按契约声明的 `500` 失败关闭 |
| `PUT /v1/display/image?fit=contain\|cover` | `image/png` 或 `image/jpeg`；`fit` 默认为 `contain` | 同步显示并返回状态 |
| `PUT /v1/display/markdown` | UTF-8 `text/markdown`，无 `Content-Encoding` 或 `identity` | 同步显示并返回状态 |
| `POST /v1/system/exit` | 空请求体 | 幂等退出独占模式并恢复原生 UI，返回状态 |

鉴权要求恰好一个 `Authorization` 头，值必须以区分大小写的 `Bearer ` 开头，其后的 token 非空且不含空格、制表符、回车、换行或逗号；比较使用常量时间（`cmd/eink-relay/main.go:528-539`）。

错误响应一律是 `{"error":{"code":"…","message":"…"}}`，不会含 token、文件路径、命令输出或请求内容。认证后的显示端点使用一个非阻塞事务锁：忙时优先返回 `409 display_busy`，不排队也不读取请求体。其余状态码和稳定错误码以 [OpenAPI](docs/openapi.yaml) 为准；特别地，进入处理器后的正文读取超时为 `408 request_timeout`，完整显示事务超时为 `504 transaction_timeout`，请求已被接收但内容无法解码或排版时为 `422`（`decode_failed` 或 `render_failed`，`main.go:789-792`）——这类失败不调用显示后端、不改动已提交画面。

参数校验同样严格：`/v1/display/image` 只接受 `fit` 一个查询参数，取值只能是 `contain` 或 `cover`，多于一个参数、`;` 分隔或非法取值均为 `400 invalid_parameter`（`main.go:693-711`）；`PUT /v1/display/markdown` 只接受可选的 `font_size`（正文字号点数，12–72，缺省 18，整份样式按比例缩放，见 `ScaledMarkdownStyle`），`POST /v1/system/exit` 不接受任何查询字符串，违规均 `400 invalid_parameter`（`main.go:655-658`、`main.go:808-811`），后者的请求体必须为空，非空为 `400 invalid_request`（`main.go:812-816`）。

方法不匹配的应答按契约各操作**已声明的响应集**分流，而不是一律 405：

- `GET /v1/status` 与 `POST /v1/system/exit` 声明了 405，非法方法返回 `405 method_not_allowed`（`cmd/eink-relay/main.go:567`、`main.go:591`）。
- `PUT /v1/display/image` 与 `PUT /v1/display/markdown` 在冻结契约里**没有** 405（`docs/openapi.yaml:90-110`、`docs/openapi.yaml:124-144`），因此非 PUT 请求与未知路径一样返回 `404 not_found`（`main.go:585-596`）——PUT 是这两个路径上唯一存在的资源。
- 未知路径返回 `404 not_found`。

另有两条容易被忽略但已实现的边界：

- `GET /v1/status`（以及两个显示端点和 `POST /v1/system/exit` 的 200 分支）在写状态行之前先把快照按冻结的 `Status` schema 自检并序列化到缓冲区；不合规就以契约已声明的 `500 internal_error` 失败关闭，绝不发出违反 schema 的 200（`main.go:869-913`）。
- 长度为 0 的 Markdown 请求体在任何渲染、任何 FBInk 调用和任何提交之前以 `400 invalid_request` 拒绝（`main.go:680-683`），因此空正文不会被光栅化成整屏白并被记成“最后一次成功画面”。

示例（不要把实际 token 写进 shell 历史、日志或仓库）：

```sh
# 取 token：唯一入口是设备上的 0600 文件，一条 ssh 即可，无需登录翻目录
EINKRELAY_TOKEN=$(ssh root@KINDLE_ADDRESS 'cat /var/local/einkrelay/token')

curl -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  http://KINDLE_ADDRESS:8080/v1/status

curl -X PUT -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  -H 'Content-Type: image/png' --data-binary @page.png \
  'http://KINDLE_ADDRESS:8080/v1/display/image?fit=contain'

curl -X PUT -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  -H 'Content-Type: text/markdown; charset=utf-8' --data-binary @page.md \
  http://KINDLE_ADDRESS:8080/v1/display/markdown

# 可选：font_size 指定正文点数（12–72，缺省 18），整份排版按比例缩放
curl -X PUT -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  -H 'Content-Type: text/markdown; charset=utf-8' --data-binary @page.md \
  'http://KINDLE_ADDRESS:8080/v1/display/markdown?font_size=32'

curl -X POST -H "Authorization: Bearer $EINKRELAY_TOKEN" \
  http://KINDLE_ADDRESS:8080/v1/system/exit
```

图片仅接受 PNG/JPEG。解码前会检查 10 MiB 编码体积、边长、像素数和保守的解码内存预算；渐进 JPEG、CMYK/YCCK JPEG 和隔行 PNG 会被拒绝。Markdown 使用 goldmark CommonMark 核心（不启用任何扩展，因此没有 GFM 表格）与本地字体自绘，不执行 HTML 或脚本、不抓取请求中引用的远程资源；不支持的内容安全降级，缺失字形或已校验字体不可用时 Markdown 请求失败而不会显示缺字符方块。超出屏幕的内容按行截断，v0.1 没有分页。

### 面板几何（`screen`）

`GET /v1/status` 报告 `screen: {width, height}`，取自请求时刻的帧缓冲探测（PW4 为 `1072×1448`）。这是**调用方唯一可靠的内容尺寸依据**：没有它，要把一张图铺满屏幕就只能靠背下设备型号，而猜错的后果不是报错——是请求返回 200、屏幕上却没有有用的东西。v0.1 曾经因为探针读错节点而把画布当成 1088×6144，`fit=contain` 的图片整个落在可见区之外，正是这一类失败。

几何探测失败时该字段为 `null`（此时任何显示请求也都会失败）。这是对冻结契约的一次**显式扩展**：`Status` schema 是 `additionalProperties: false`，因此 [docs/openapi.yaml](docs/openapi.yaml) 已同步加入 `screen` 与 `ScreenSize`，并把 `screen` 列入 `required`。忽略未知字段的既有客户端不受影响。

## 默认配置

无效配置会在监听前失败。下列变量均为可选；默认值即实现的默认值，逐条对照源码核验（定义位置与更多细节见 [docs/configuration.md](docs/configuration.md)）。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `EINKRELAY_LISTEN_ADDRESS` | `0.0.0.0` | USBNetwork/Wi-Fi 共享监听地址 |
| `EINKRELAY_LISTEN_PORT` | `8080` | 监听端口 |
| `EINKRELAY_STATE_DIR` | `/var/local/einkrelay` | 持久状态目录；设置后会同时重定位默认 token、socket、activity 路径 |
| `EINKRELAY_TOKEN_PATH` | `/var/local/einkrelay/token` | 普通、非符号链接且精确 `0600` 的 token 文件 |
| `EINKRELAY_GUARDIAN_SOCKET` | `/var/local/einkrelay/guardian.sock` | Guardian Unix socket |
| `EINKRELAY_ACTIVITY_PATH` | `/var/local/einkrelay/activity.json` | 独占/主动退出持久意图 |
| `EINKRELAY_INPUT_DEVICE` | `/dev/input/event2` | 只读触摸输入；配置校验拒绝 `event0`/`event1`，打开器进一步只接受基名恰为 `event2` 的非符号链接字符设备，因此不读取、抓取或重映射电源键 |
| `EINKRELAY_FBINK_PATH` | `/mnt/us/einkrelay/bin/fbink` | 外部 FBInk 可执行文件 |
| `EINKRELAY_FONT_DIR` | `/mnt/us/einkrelay/fonts` | 字体与 manifest 所在目录，必须是绝对路径 |
| `EINKRELAY_FONT_MANIFEST` | `<font dir>/manifest.json` | 仅覆盖 manifest 位置 |
| `EINKRELAY_IMAGE_MAX_BYTES` | `10485760`（10 MiB） | 图片编码请求上限 |
| `EINKRELAY_MARKDOWN_MAX_BYTES` | `1048576`（1 MiB） | Markdown 请求上限 |
| `EINKRELAY_IMAGE_MAX_DIMENSION` | `8192` | 声明图像单边上限 |
| `EINKRELAY_IMAGE_MAX_PIXELS` | `32000000` | 声明图像像素上限 |
| `EINKRELAY_IMAGE_MAX_DECODED_BYTES` | `50331648`（48 MiB） | 解码前最坏情况内存预算；取值由设备实测确定，见下 |
| `EINKRELAY_READ_TIMEOUT` | `15s` | HTTP header 与请求体读取期限 |
| `EINKRELAY_TRANSACTION_TIMEOUT` | `60s` | 同步显示事务总期限 |
| `EINKRELAY_GESTURE_TAP_WINDOW` | `1s` | 任意角落连点 3 次触发退出的时间窗 |
| `EINKRELAY_LIFECYCLE_TIMEOUT` | `10s` | 进入/离开独占模式与 Guardian 控制 socket 往返的期限（PW4 实测退出约 0.3s） |

上表即实现读取的全部环境变量，共 19 个，没有未文档化的变量。取值格式：超时类使用 Go `time.ParseDuration` 语法（如 `15s`、`1m`），端口与字节/像素类为十进制整数，其余为字符串（地址或绝对路径）。空字符串等同于未设置，保留默认值。

`scripts/install.sh`、`start.sh`、`stop.sh` 与 `uninstall.sh` 管理的是固定的设备布局 `/mnt/us/einkrelay` 和 `/var/local/einkrelay`，这是可审计、可撤销的安装路径。它们会继承其他环境设置（例如监听地址、端口和显示限制），但**不支持**通过 `EINKRELAY_STATE_DIR`、`EINKRELAY_TOKEN_PATH`、`EINKRELAY_GUARDIAN_SOCKET` 或 `EINKRELAY_ACTIVITY_PATH` 改写其管理位置。需要非默认持久路径时，请先在受控环境中直接以同一组变量运行 `eink-relay resume`，再运行 `eink-relay guard`；不要混用默认脚本和自定义状态路径。

热区是屏幕短边 15% × 15% 的角部方块，**四个角各一个**（`cmd/eink-relay/gesture.go:188-203`）。触发条件是**单指在同一个角落、1 秒内连点 3 次**（时间窗见 `main.go:93`）：点按落在不同角落、落在热区外、超过时间窗，或出现第二根手指，都会把计数清零。长按不触发。独占期间触摸流由 `EVIOCGRAB` 独占接管（`gesture.go:126-150`），因此这三次点按不会同时被底下仍然活着的原生界面收到。Guardian 在 60 秒滑动窗口内累计五次“启动失败”后进入 failsafe 并停止自动重启（`cmd/eink-relay/guardian.go:362`、`guardian.go:355`）；这里的“启动失败”专指服务存活不足 10 秒即退出（`guardian.go:348`），已正常服务一段时间后再退出算作普通重启，重启退避为 1s/2s/4s/8s、此后固定 8s（`guardian.go:316`、`guardian.go:367-379`）。触发 failsafe 后 Guardian 把面板还给原生界面并以状态码 1 退出，需要显式重新启动（KUAL 菜单或 `/mnt/us/einkrelay/start.sh`）。

把 `EINKRELAY_INPUT_DEVICE` 指向 `event2` 以外的节点会让手势监视器无法打开设备：它会持续重试重开，重试间隔从 250 毫秒按 2 倍退避到 5 秒封顶（`gesture.go:551-552`、`gesture.go:563-572`），Guardian 与 REST 退出路径不受影响（`gesture.go:98`、`gesture.go:695`、`gesture.go:695`）。日志是**收敛**的：首次失败、原因变化和恢复各记一条，中间的重复只在每 5 分钟汇总一次并附带被抑制的次数（`gesture.go:558`、`gesture.go:591-616`）——不加节流时一个缺失的触摸节点每秒会写 4 行，一天约 35 万行，而这份日志位于 Kindle 容量很小的根分区上。

## 显示延迟与帧体积

图片解码预算按设备实测设定，而不是估一个整数。输出永远是 1.55 MP 的整屏帧，因此更大的预算只买到"调用方不必先缩图"这点便利。实测（PW4，每次请求前复位服务，因此峰值是单次真实值）：

| 请求 | 预算估算 | 结果 | 服务峰值 RSS | MemAvailable |
| --- | --- | --- | --- | --- |
| 空闲 | — | — | 20 MB | 167 MB |
| 2000×1500（3.0 MP） | 12 MB | 200 | 35 MB | 155 MB |
| 2500×2500（6.2 MP） | 25 MB | 200 | 43 MB | 147 MB |
| 3500×3500（12.2 MP） | 49 MB | 200 | 76 MB | 约 111 MB |
| 4200×4200（17.6 MP） | 71 MB | 413 | — | — |

**这里有一处判断失误，如实记录**：中途有一版把预算砍到 32 MiB，理由是"12.2 MP 那次设备只剩 10 MB"。那是读错了指标——`free` 列不含可回收的页缓存，真正决定 OOM 的是 `MemAvailable`，它全程保持在 110 MB 以上。并不存在那道悬崖；为了躲一道不存在的悬崖而拒绝一张很普通的 1200 万像素手机照片，是笔坏交易。

最终取 **48 MiB**，仍比最初的 64 MiB 紧，但理由换成另一个、且只在这次独占模式重设计之后才成立的事实：**原生界面不再被停掉**。Xorg、awesome 与 framework 守护进程全程驻留，随时可能因为打开一本书或弹一个对话框而分配内存——峰值余量不再由本进程独占。48 MiB 放行 12.2 MP（峰值 76 MB），拒绝 17.6 MP，正是估算与实测都显示曲线开始变陡的位置。需要更大的调用方可以显式抬高 `EINKRELAY_IMAGE_MAX_DECODED_BYTES`。

估算模型本身经复测确认是准的：上表范围内真实峰值约为声明估算的 1.15 倍。

字体 face 缓存有上界（`fonts.go` 的 `maxCachedFaces`）。缓存按 `(角色, 字号)` 计键，而可选的 `font_size` 跨越 12–72，每个取值都会产生新的正文、等宽和六级标题字号；没有上界时，一个做响应式排版的客户端会把整个参数空间走一遍，并把途经的每个 face 钉住到进程结束。上界取 32，远高于单个文档约 12 个 face 的工作集，因此排版过程中不会发生驱逐。

这个上界的实际收益经过实测，结论**比看上去小**，如实记录：遍历全部 61 个字号时，无上界会让 serve 常驻内存从 22.8 MB 涨到 56.6 MB，有上界则稳定在约 51–54 MB。差值真实但有限——其中大部分增长其实是 Go 堆保留了渲染路径的峰值工作集（一整屏画布加编码缓冲），而不是缓存本身：重复遍历不会继续增长，反复用同一个字号推 60 次更是**一 KiB 都不涨**。加这个上界的理由是「以请求参数为键的无界缓存本身就是结构性缺陷」，而不是「它能省下几十 MB」——它省不了。

一次显示请求的耐久性开销是可观的，值得写清楚它由什么构成。事务把候选帧写盘并 fsync、快照两代画面、依次写入 5 个事务阶段标记（每个都是"临时文件 → fsync → rename → 目录 fsync"）、调用 FBInk、轮换、提升、再同步目录——PW4 实测 fsync 边际成本约 **5 ms**，整屏 FBInk 刷新约 **100 ms**。

其中一项曾经是纯浪费：为了挑选回滚基线，事务会把 `current.png` 与 `previous.png` **完整解码**（崩溃恢复时多达 5 次）。这些文件在写入时早已经过完整解码校验，此处真正要确认的只有两件事——字节没变、几何仍然匹配。PNG 每个 chunk 自带 CRC32，IHDR 自带几何，因此改为走 chunk 校验：**3.75 ms → 1.87 µs，快约 2000 倍**，且不改变磁盘格式、不需要迁移、不弱化任何保证（`persist.go` 的 `verifyStoredFullScreenPNG`）。写入路径仍然做完整解码，深检查留在它该在的地方。

另有两处 `syncDir` 已移除：它们只让"删除临时文件"这件事变得可崩溃恢复，而阶段标记本身已经是持久的，崩溃后下一次对账会重新删一遍。


显示是**同步**的：请求在解码、转灰、缩放、编码、落盘 fsync 和 FBInk 调用全部完成之前不会返回，全程跑在 PW4 的单核 ARMv7 上。因此这条流水线上的每一步都直接是调用方看到的等待时间。仓库内的基准（`cmd/eink-relay/render_bench_test.go`）用来在改动这条路径时给出可复算的比例依据：

```sh
EINKRELAY_FONT_DIR=/path/to/fonts go test -run '^$' -bench . ./cmd/eink-relay
```

两处刻意的取舍，数值均取自上述基准在开发机（darwin/arm64）上的观测——**绝对值在 PW4 上会慢一个数量级以上，可用的是比例**：

- **灰度转换走按类型的快路径**。通用实现按像素调用 `image.Image.At` 再 `RGBA()`，每个像素付一次接口分发和一次颜色装箱。快路径直接读像素缓冲，然后把样本交给**同一个** `color.Color.RGBA` 方法——去掉的是分发，不是颜色的定义。实测 NRGBA（PNG 常见产物）27.3 ms → 3.4 ms，YCbCr（JPEG 产物）32.6 ms → 9.9 ms。等价性由 `TestGrayFastPathsMatchTheGenericConversion` 逐像素断言：把算术照抄一份而不是复用 `RGBA()`，恰好会在 YCbCr 上因舍入精度不同而整体移动像素值，这一点在开发过程中被该测试实际抓到过。
- **整屏 PNG 用 `png.BestSpeed` 而不是 `png.DefaultCompression`**（`image.go:368-370`）。这一帧是交给同机 FBInk 消费的临时候选文件，压缩率只换来略小的一次写入，而压缩**时间**直接坐在同步请求路径上。代表性帧（整屏文本，即 Markdown 请求的产物）实测 9.95 ms/71 784 B → 5.57 ms/75 518 B：**快 44%，只大 5%**。整屏高熵图像是最坏情况：32.8 ms/57 845 B → 9.50 ms/196 883 B，时间省下约 3.5 倍，代价是每帧多写约 139 KB——仍被"一屏 8 位灰度"这个上限框住，且这些文件是瞬时的候选帧。

PNG 是无损的，所以"更快"永不等于"不同"：`TestScreenPNGEncoderRoundTripsEveryFrame` 断言编码结果仍解码回完全相同的样本，CJK 黄金测试的**像素**摘要在这次编码器改动前后一字未变。

## 构建与本地验证

需要 Go 和用于 CJK 黄金测试的、与 `assets/fonts/manifest.json` 匹配的字体目录。仓库不提交大字体二进制；在受控验证环境中设置 `EINKRELAY_FONT_DIR` 指向已校验字体目录。

```sh
EINKRELAY_FONT_DIR=/path/to/fonts scripts/check.sh
```

该命令明确运行 CJK 混排黄金测试（并要求它显式 PASS，而不是被跳过）、全量 `go test`，并以 `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7` 交叉构建到临时目录；它不会把构建产物写进仓库。可复现的 release 构建使用：

```sh
EINKRELAY_FONT_DIR=/path/to/fonts \
  scripts/release-build.sh /absolute/path/outside/the/repository
```

该脚本先跑完整验证，再以 `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7` 构建唯一支持的目标，写出 `eink-relay`、`eink-relay.sha256` 与 `eink-relay.buildinfo`，并从磁盘重新读回产物校验摘要。构建时会把 `git describe --tags --always --dirty` 的结果作为 `main.version` 打进二进制，因此 `/v1/status` 的 `version` 能指认设备上究竟装的是哪一版；带 `-dirty` 说明它是从未提交的工作树构建的。非 release 构建保持编译期默认值 `dev`。输出目录必须是绝对路径：仓库根目录一律拒绝；仓库内的子目录只有在 `git check-ignore` 能证明这三个产物都被忽略时才接受（仓库内唯一满足该条件的是已加入 `.gitignore` 的 `dist/`）。脚本不提交、不打 tag、不 push、不发布 Release。详见 [docs/release-build.md](docs/release-build.md)。

产物须与 FBInk 资产、`assets/fonts/manifest.json` 一起组成安装包；安装时要求由操作者提供 FBInk 的公开 SHA-256。`go test`、黄金图、并发/事务、损坏回退、崩溃恢复和手势测试均为本地验证，不替代设备证据。

## Kindle 安装、启动、停止与卸载

仅在已授权的 Paperwhite 4 上以 root 运行下列操作；四个脚本都会先检查 `id -u`。安装不写 `/etc`、不加入开机钩子、不自动启动，也不修改 KUAL、KOReader、USBNetwork、SSH 或 KindleForge。

### 安装

安装包根目录必须同时包含 `eink-relay`、`bin/fbink` 和 `assets/fonts/manifest.json`，缺一即失败。脚本从自身所在目录的上一级推导包根目录（`install.sh:9-10`），因此 `install.sh` 必须留在安装包的 `scripts/` 下；用相对还是绝对路径调用、当前工作目录在哪里都不影响判定：

```sh
# 在安装包根目录；参数是随包 FBInk 的 SHA-256（64 位十六进制）
scripts/install.sh <fbink-sha256>
```

`install.sh` 的实际动作，按顺序：

1. 创建 `/mnt/us/einkrelay`、`/mnt/us/einkrelay/bin`、`/mnt/us/einkrelay/fonts`（`0755`）和 `/var/local/einkrelay`（`0700`），并以 `umask 077` 运行。
2. 复制 `eink-relay`（`0755`）、`bin/fbink`（`0755`）和 `assets/fonts/manifest.json`（`0644`）到上述目录。
3. 运行 `eink-relay preflight -sha256 <fbink-sha256>`：校验 FBInk 是普通可执行文件、摘要匹配，并能成功执行 `--help`（上游 FBInk 没有 `--version` 选项，未知选项会以非零码退出）。
4. 运行 `eink-relay fonts ensure`：按 manifest 获取并校验固定字体。若设备可访问 manifest 指定的 HTTPS 来源则下载；否则可按下方离线预置步骤提供同一已校验文件。失败即整体失败，不留半成品。
5. 若 `/var/local/einkrelay/token` 不存在，用 `/dev/urandom` 取 32 字节熵、编码为 64 个小写十六进制字符写入该文件，**不带尾随换行**（`install.sh:58-62`；多一个 `\n` 就会因不可打印字节而被 `LoadToken` 拒绝）。已存在的 token 不会被覆盖，但 `chmod 0600` 每次安装都会执行（`install.sh:66`）；token 路径是符号链接时直接失败。四个脚本都不打印 token，也不把它写进收据、日志或命令行参数——`start.sh` 甚至完全不读取它的内容，只检查路径与权限（`start.sh:17-19`）。
6. 写出安装收据 `/var/local/einkrelay/install.receipt`（`0600`），内容是固定的文件清单：程序、fbink、manifest、字体文件、`start.sh`、`stop.sh`、token、收据自身；设备上已装 KUAL 时再加上启动项的三个文件（`menu.json` 与两个 `bin/*.sh`）。清单是固定集合，不是任意路径——`uninstall.sh` 据此校验。

安装不启动任何进程，也不创建 `activity.json`、`guardian.sock` 或画面文件——这些由运行时写入。

若 Kindle 缺少可用 CA 证书导致第 4 步失败，可在联网主机按 manifest 中的 URL 获取字体、以 `sha256sum` 核对后拷贝到 `/mnt/us/einkrelay/fonts`，再运行：

```sh
EINKRELAY_FONT_DIR=/mnt/us/einkrelay/fonts \
  /mnt/us/einkrelay/eink-relay fonts ensure
```

### 启动

```sh
scripts/start.sh
```

**从 Kindle 原生界面启动（KUAL 入口，安装时自动写入）**：设备上已存在 `/mnt/us/extensions` 时，`install.sh` 会把启动项一并装好，KUAL 菜单里直接出现「EInkRelay: 启动」与「EInkRelay: 停止并恢复界面」两项，日常开关不需要 SSH。它**只在 KUAL 已存在时安装**，绝不自行创建 `extensions` 根目录——在没有 KUAL 的设备上凭空造一个没人拥有的目录不是安装该做的事。启动项从**已安装目录** `/mnt/us/einkrelay/start.sh` 运行，而不是解包出来的安装包目录：后者是临时暂存区，操作者装完随时可以删掉，一个会因此失效的启动入口不算启动入口。EInkRelay 的安装与运行仍然不依赖 KUAL（合同边界不变：不写 `/etc`、不加开机钩子、不自动启动）。启动项**以控制 socket 出现为准判定成功**（最多等 30 秒，进程中途退出则立即失败），而不是"睡 2 秒再看进程在不在"：socket 只有在 Guardian 走完配置、状态与独占进入之后才存在，因此它是唯一诚实的就绪信号——进程还在但马上会因 token 或 FBInk 问题退出的情况不会再被报成启动成功。

启动项还要区分「进程在」和「面板是我们的」这两件事。一次 REST 或角落退出之后，Guardian **仍在运行**并继续监督服务，只是 activity 记为 inactive、面板已还给原生界面。若把这种状态当成"已经启动"，菜单项就会静默地什么都不做——用户会以为入口坏了。因此启动项只在 activity 确实为 `active:true` 时才当作无事可做；否则先终止现有 Guardian 再干净地重启一次（重新进入独占的决定是 Guardian 启动时依据 activity 记录做出的，所以只能靠重启来重新做这个决定）。

`start.sh` 检查程序可执行、token 存在且不是符号链接并重置为 `0600`，然后执行 `eink-relay resume`（把 activity 记为 active，这是唯一会置 active 的入口）并 `exec eink-relay guard`。Guardian 在前台运行：它进入独占模式、监听控制 socket、启动角落手势监视，并以子进程方式监督 `eink-relay serve`。因为是前台进程，在该终端按 Ctrl-C（SIGINT）或向它发送 SIGTERM 会让它把面板还给原生界面后退出，但不会清除 active 记录——下次启动仍会重新进入独占模式。

首次启动（或历史上从未成功显示过任何画面）时，没有可恢复的 `current.png`/`previous.png`，服务会把**内置帮助页**——token 获取方法、图片/Markdown 推送示例、角落连点退出说明、状态查询端点——通过正常显示事务渲染为第一张成功画面（`cmd/eink-relay/default_page.go`），而不是留下一块像死机一样的白屏；此后它和其他画面一样参与持久化与重启恢复，用户推送的第一张内容会正常覆盖它。字体库不可用时仍保持原行为：不动屏幕并把诊断写进 `/v1/status`。

**只有 SIGINT 与 SIGTERM 被处理**（`cmd/eink-relay/main.go:1217`）。直接关闭终端或断开 SSH 会话发出的是 SIGHUP，它不在处理列表内：Guardian 与被它监督的 `serve` 会随前台进程组一起被杀掉，**不会**把面板还给原生界面，设备停留在独占模式。因此请始终用角落连点、`POST /v1/system/exit`、KUAL 的停止项或 `scripts/stop.sh` 结束，而不是关窗口；需要长时间离线运行时，请在 `screen`/`tmux` 或 `setsid`、`nohup` 之类不会转发 SIGHUP 的会话里启动。万一已经这样断开，重新登录后运行 `scripts/stop.sh` 即可恢复原生 UI。

### 停止与恢复

```sh
scripts/stop.sh
```

`stop.sh` 是恢复操作，不是进程杀手。它优先通过 `/var/local/einkrelay/guardian.sock` 发送 `EXIT`（需要支持 Unix socket 的 `nc`），并要求回复恰好为 `OK` 才算成功；此时 Guardian 已把面板还给原生界面并把 activity 记为 inactive，但 Guardian 本身仍在前台继续监督服务，`/v1/status` 与 `/v1/system/exit` 保持可达。socket 不存在或握手失败时才走本地后备，后备与 `Lifecycle.Exit` 做的是同一件事、同样的顺序：`lipc-set-prop com.lab126.powerd preventScreenSaver 0`，然后把进入独占前存下的那一帧写回面板。

**独占模式不再停止任何 upstart job。** 这一点是本版本最大的行为变更，值得单独说明，因为旧做法在真机上是不可逆的：

- 旧做法停掉 `lab126_gui` 与 `framework`，退出时再启回来。问题一，job 的状态不是界面的真实情况——`status lab126_gui` 会报 `start/running` 而背后根本没有进程，于是每一次"确认恢复成功"读到的都只是账面。问题二也是决定性的：5.18 上原生界面是 **Xorg + awesome + blanket**（后者托管 `com.lab126.KPPMainApp`）这样一套**事件驱动重绘**的栈，它只在发生了它知道的事情时才重画。我们往 framebuffer 写一帧，它无从知晓自己的像素已被覆盖，因此**永远不会重绘**——这一点在**什么都不停**的情况下被单独证实过：原生栈完好运行时画一帧上去，面板会一直停在那一帧。既然如此，循环任何 job 都修不好它。
- 现在的做法：进入时**保存原生界面当时那一帧**，退出时**把它原样写回去**。这不需要原生栈的任何配合，而且比"请它重绘"更正确——独占期间触摸流是被独占接管的，底下的界面从未前进过，所以存下来的那一帧就是它的真实状态。之后用户的第一次真实交互会自然重绘并刷新掉时钟之类的陈旧信息。
- 曾经逐个在真机上对着被覆盖的面板试过、并逐帧比对确认**无效**的动词：pillow 的 `disableEnablePillow` / `displayChrome` / `interrogatePillow`、appmgrd 的 `startdefault`、powerd 的 `outOfScreenSaver` 事件。powerd 的 `wakeUp` 确实能重绘，但**只在设备真正休眠时**；醒着时它返回 `lipcErrNoSuchProperty`，而退出恰恰总是在醒着的状态下发生。注意 `lipc-set-prop` 对不存在的属性同样返回 0，**它的退出码不构成任何证据**。

由于原生界面全程活着，它也会收到触摸事件。因此独占期间手势监视器用 `EVIOCGRAB` **独占接管触摸流**（`gesture.go` 的 `Grab`），否则角落三连点会同时被底下看不见的界面收到、把它导航到别处，等面板还回去时用户就落在了自己没要求过的地方。抓取只在面板确实被覆盖时持有，且描述符关闭时由内核自动释放，因此 Guardian 崩溃或被杀都不会把触摸屏留给一个已死的进程。

**进入独占前如果无法保存那一帧，就不进入。** 这是本次重新设计要确立的硬不变式：只有已经证明自己能把面板还回去的服务，才有资格接管面板。

保存下来的那个文件（`/var/local/einkrelay/panel-snapshot.png`）**不是缓存，而是「我们还欠用户一块屏幕」这笔债的持久凭证**。这个区分决定了几件事的对错：

- Guardian 在独占期间被 SIGKILL，凭证留在磁盘上。下一个 Guardian 看到债务未清，**不会重新捕获**——那时 framebuffer 上是我们自己的内容，再捕获等于把欠着的原生帧换成欠债的原因本身，用户将永远拿不回自己的界面。它会在下一次退出时把这笔债还掉。
- 反过来，一个从未覆盖过面板的 Guardian（只是监督服务）**什么都不欠**，它的退出没有东西要还，因此不能报失败。否则全新安装上的第一次 `POST /v1/system/exit` 会返回 500，SIGTERM 关闭也会以非零码退出。
- 还款失败则债务保留，由下一次退出（重试、角落手势或 Guardian 关闭）继续尝试，而不是被悄悄遗忘。

角落三连点和 `POST /v1/system/exit` 走的是同一条退出协调路径，均幂等。要真正结束 Guardian，请在其前台终端中断它、向该进程发送 SIGTERM，或使用 KUAL 菜单里的「停止并恢复界面」。

服务崩溃或设备重启后，若 activity 仍为 active、且既没有 failsafe 闩锁也不是「恢复未完成」标记（`guardian.go:253-255`），Guardian 才会重新进入独占模式，`serve` 会重新校验并重新显示最后一次成功的画面；主动退出（REST、手势、`stop.sh`）以及触发过 failsafe 之后重启都不会自动重入，需要再次启动。

四条恢复路径按可用性排序，任何一条成功都会把面板交还原生界面，且互相幂等：

1. 任意角落 1 秒内连点 3 次（不需要网络、不需要终端）。并发触发（REST、手势、重复手势）由 `ExitCoordinator` 的 singleflight 合并为同一次执行。**时序（PW4 实测）**：整个退出约 **0.3 秒**，面板逐像素还原为进入独占前的那一帧。旧版本那条"退出约 25 秒、原生 UI 重绘可能再滞后 1~2 分钟"的说明已经不适用，同样地那张「再见 · Goodbye」等待提示页也已移除——恢复比渲染一张提示页还快，再显示它只会凭空多花时间并承诺一个不会发生的等待。
2. `POST /v1/system/exit`（需要网络与 token；由 Guardian 执行恢复）。
3. `scripts/stop.sh` 或 KUAL 菜单项（需要 root；Guardian 在时走控制 socket，不在时走本地后备）。
4. 最后手段：硬件长按电源键重启。安装不写 `/etc`、不加开机钩子、不自动启动，也不重映射电源键，因此重启后设备停在 Kindle 原生界面，EInkRelay 不会自行启动——除非再次显式启动它。

### 卸载

```sh
scripts/uninstall.sh
```

先停止服务再卸载。`uninstall.sh` 只读取安装收据，并且要求收据恰好是 `install.sh` 写出的那份固定清单（KUAL 启动项的三个文件是**全有或全无**：只列出其中一两个的收据不是这个安装器写的，直接拒绝），任何多余、重复或陌生的路径都会让它整体拒绝执行；随后逐条 `rm -f` 清单里的文件。它不递归删除、不删除 `/mnt/us` 或 `/var/local` 下的共享根目录，也不会删除 `activity.json`、`guardian.sock`、`current.png`、`previous.png` 等运行期文件——这些需要在确认不再需要后手动清理。

## 故障排查

| 现象 | 检查与恢复 |
| --- | --- |
| `/v1/*` 返回 401 | 先确认 token 本身已取对：`EINKRELAY_TOKEN=$(ssh root@<设备> 'cat /var/local/einkrelay/token')`。再确保恰好一个 `Authorization` 头、方案拼写为区分大小写的 `Bearer ` 且后跟一个空格，token 非空且不含空格、制表符、回车、换行或逗号；确认设备 token 文件存在、非符号链接、模式为 `0600`。不要在终端记录中粘贴 token。 |
| `serve` 打印 `token validation failed` 并退出 | token 文件必须是普通文件、权限恰好 `0600`、长度 32–512 字节，且每个字节都是 `0x21`–`0x7e` 的可打印 ASCII。任何尾随换行、空格或其他不可打印字节都会让校验失败。 |
| 服务反复重启后不再启动 | 这是 60 秒窗口内五次“启动不足 10 秒即退出”触发的 failsafe，Guardian 已把面板还给原生界面并退出。先按上一行排查 token，再排查 FBInk、字体或配置，然后从 KUAL 菜单或 `/mnt/us/einkrelay/start.sh` 重新启动。 |
| Markdown 返回 `500 display_failed`（“the pinned fonts are unavailable”），或 `/v1/status` 持续报字体错误 | 固定字体缺失、摘要不符或 manifest 无法读取（`main.go:795-798`）。运行 `eink-relay fonts ensure`；确认字体文件的大小与 SHA-256 和 manifest 一致。设备无法联网时按上面的离线预置流程处理。服务**失败关闭**而不是渲染一页缺字符方块。图片端点与退出路径在此期间仍可用；这是持久错误，成功显示一张图片不会把它清掉（`main.go:1112`）。 |
| `422`（`render_failed` / `decode_failed`） | `render_failed`：已装载的字体没有某个必需字形，或 Markdown 无法排版（`main.go:791-792`）；`decode_failed`：PNG/JPEG 数据本身损坏或不是声明的格式。两者都不调用 FBInk、不清屏，`current.png`/`previous.png` 不变。 |
| 安装前检查失败 | 确认包内 `eink-relay`、`bin/fbink`、`assets/fonts/manifest.json` 都在，且传给 `install.sh` 的 FBInk SHA-256 正确；`eink-relay preflight -sha256 …` 会单独检查可执行位、摘要和 `--help` 能否运行。 |
| 显示端点返回 `404 not_found`（路径拼写正确） | 方法用错了。`/v1/display/image` 与 `/v1/display/markdown` 只接受 `PUT`；由于契约没有给这两个操作声明 405，其他方法与未知路径同样是 `404 not_found`。不要据此判断服务未安装。 |
| `400 invalid_request`（Markdown） | 请求体为空。契约把该请求体标为必需，空正文在渲染与提交之前即被拒绝，屏幕与 `current.png` 均未改动。 |
| `400 invalid_encoding`（Markdown） | 请求体不是合法 UTF-8。只接受 UTF-8；服务不做字符集转换。 |
| `400 invalid_parameter` | 图片端点带了 `fit` 以外的查询参数、多个参数、`;` 分隔或 `fit` 取值不是 `contain`/`cover`；或在 Markdown、`/v1/system/exit` 上带了任何查询字符串。 |
| `400 invalid_request`（`/v1/system/exit`） | 该端点要求空请求体，任何字节都会被拒绝。 |
| `GET /v1/status` 返回 `500 internal_error` | 组装出的状态快照不满足冻结的 `Status` schema（例如后端上报了枚举外的 `state`），服务选择失败关闭而不是发出违反契约的 200。检查 FBInk 是否可执行、`--help` 是否正常，并查看 Guardian 日志。 |
| `/v1/status` 的 `backend.state` 不是 `ready` | 后端名固定为 `fbink`。`unavailable` 表示 `EINKRELAY_FBINK_PATH` 指向的文件不存在、不是普通文件或没有可执行位；`error` 表示上一次显示尝试失败（`render.go:111-123`）。检查该路径与可执行位，并用 `eink-relay preflight -sha256 …` 复核摘要与 `--help`。 |
| 显示请求返回 `500 display_failed` | FBInk 调用失败、帧缓冲几何探测失败或返回非法尺寸（`main.go:753-757`、`main.go:799-801`）。屏幕保持上一张成功画面。检查 FBInk 可执行、`/v1/status` 的 `backend`，以及 Guardian 日志。 |
| `500 persistence_failed` | 候选画面无法被写入、校验或原子提交到 `EINKRELAY_STATE_DIR`。检查该目录是否存在、可写且未满；提交失败时不会有半张画面被记为最后一次成功画面。 |
| `409 display_busy` | 前一个同步显示事务仍在运行；稍后重试。服务不排队、不创建后台渲染。 |
| 显示成功返回 `200`，但响应体里 `busy` 是 `true` | 这是预期行为，不是故障。`busy` 报告的是"此刻显示事务锁被持有"，而一次成功的 PUT 正是在自己仍持锁时序列化出这份状态快照的。判断服务是否空闲请用**另一个** `GET /v1/status`，不要读自己那次显示响应里的 `busy`。 |
| `413 payload_too_large` | 请求体超过 `EINKRELAY_IMAGE_MAX_BYTES`（默认 10 MiB）或 `EINKRELAY_MARKDOWN_MAX_BYTES`（默认 1 MiB）。 |
| `408 request_timeout` | 进入处理器后的请求体读取超过 `EINKRELAY_READ_TIMEOUT`（默认 15 秒）。检查链路带宽与客户端是否在慢速发送大图。 |
| `504 transaction_timeout` | 整个同步显示事务超过 `EINKRELAY_TRANSACTION_TIMEOUT`（默认 60 秒）。检查 FBInk 是否卡住；超时时屏幕保持上一张成功画面，`current.png`/`previous.png` 不变。 |
| `413 image_dimensions_exceeded` | 减小图片边长、像素数或解码内存需求；改用非隔行 PNG、非渐进且非 CMYK/YCCK 的 JPEG。 |
| `415 unsupported_media_type` / `unsupported_content_encoding` | 图片只接受 `image/png`、`image/jpeg`；Markdown 只接受 `text/markdown`（charset 缺省或 `utf-8`）。`Content-Encoding` 必须缺省或 `identity`，服务不解压请求体。 |
| `POST /v1/system/exit` 返回 `500 lifecycle_failed` | Guardian 未运行或控制 socket 不可用——退出由 Guardian 独家执行，服务不做第二套本地恢复。以 root 运行 `scripts/stop.sh` 使用后备恢复路径。 |
| 推送图片返回 200，但屏幕一片空白 | **v0.1 的缺陷，已修复**。几何探针曾读 `/sys/class/graphics/fb0/virtual_size`（PW4 上是 1088×6144，即补齐后的行宽与若干块堆叠缓冲），把虚拟几何当成了面板。于是每一帧都排在比屏幕高 4.2 倍的画布上：`fit=contain` 的图片被居中到可见区**之下**，整屏因此全白。现在读 `modes`（`U:1072x1448p-0`）。若仍遇到，确认 `modes` 节点存在且首行可解析——探针**失败关闭**，不会退回 `virtual_size`。 |
| 退出后回不到原生界面 | **v0.1 的缺陷，已修复**。旧实现停掉 `lab126_gui`/`framework` 再启回来，而这个固件的原生界面是事件驱动重绘的，它并不知道自己的像素被覆盖了，因此永远不会重画。现在改为进入时保存那一帧、退出时原样写回。若仍遇到：检查 `/var/local/einkrelay/panel-snapshot.png` 是否存在——它不存在意味着**根本不该进入过独占模式**（无法保存即拒绝进入），请查 Guardian 日志。 |
| 无法退出独占模式 | 单指在任意一个角落 1 秒内连点 3 次；无效时以 root 运行 `scripts/stop.sh`。电源键从未被打开或重映射，硬件长按重启路径始终可用。 |
| 终端断开后屏幕仍被独占、服务已不在 | SIGHUP 未被处理（只处理 SIGINT/SIGTERM），Guardian 被连带杀掉，来不及把面板还回去。重新登录后以 root 运行 `scripts/stop.sh` 走本地后备恢复（它会把进入独占前存下的那一帧写回面板）；下次改用 `screen`/`tmux`/`setsid` 启动。 |
| 角落连点无反应 | 手势要求**同一角落**（短边 15% 的角部方块，四个角均可）在 1 秒内完成 3 次点按；点按落在不同角落、落在热区外、超过时间窗或出现第二根手指都会重置计数，长按不再触发。触摸节点或帧缓冲暂时不可读时监视器会持续重试重开（250 毫秒起、2 倍退避、5 秒封顶），不会拖垮 Guardian 也不会刷屏写日志，此时 REST 退出路径仍然可用。若 Guardian 日志反复出现 `the touch device is unavailable`，检查 `EINKRELAY_INPUT_DEVICE` 是否仍指向真实的 `/dev/input/event2`。PW4 实机另有一个坑已修复：原生 framework 被停止后会把 goodix 触摸控制器留在加锁睡眠态（`/proc/touch` 显示 `Touch is locked`），事件流完全归零；进入独占模式时 `Lifecycle` 会自动向该节点写入 `unlock`（`lifecycle.go` 的 `unlockTouch`），非 goodix 设备无此节点则跳过。 |
| current 画面损坏 | 启动恢复会先校验 `current.png` 再回退 `previous.png`；两者都无效时不清屏、不显示未验证数据，先查 `/v1/status` 中脱敏的错误信息。 |
| 连接被拒绝或超时（没有 HTTP 响应） | 服务未监听。确认 `guard` 仍在前台运行、监听地址与端口正确，并确认 USBNetwork 或 Wi-Fi 链路本身可达。服务不会返回 `503`——它没有这个状态码。 |

## PW4 硬件验收

真实硬件验收只能在授权的 Kindle Paperwhite 4 上进行，且必须与本地测试记录分开。执行者应记录设备型号/固件、时间、安装包 SHA-256、FBInk SHA-256、网络路径和每项结果，不记录 Bearer token。

| 项目 | 设备操作与预期证据 |
| --- | --- |
| 安装与启动 | 完成 `install.sh`、`start.sh`；确认不依赖 KUAL，且现有 SSH、USBNetwork、KOReader 保持可访问。 |
| USBNetwork 与 Wi-Fi | 分别使用同一五端点契约调用；记录地址、状态码和脱敏响应，不记录 token。 |
| 鉴权 | 无匿名端点；缺失/错误 token 的 `/v1/*` 返回 401，且屏幕与当前画面未变。 |
| PNG/JPEG 与 contain/cover | 两种格式、两种 fit 均为正确全屏灰度画面；非法/超限输入被拒绝且不清屏。 |
| Markdown | 中英文标题、段落、列表、引用和代码块可读；HTML/脚本/远程内容未执行或抓取。 |
| 持久恢复 | 成功显示后重启服务，恢复 current；破坏 current 后确认回退 previous。 |
| 退出手势 | 分别验证四个角的 1 秒三连点退出；普通单点、跨角点按、慢速点按、长按和多指均不误触。 |
| 原生恢复与电源键 | `POST /v1/system/exit`、手势及 `stop.sh` 均恢复原生 UI；不打开或重映射电源键，硬件长按重启路径不受干预。 |
| 重启恢复 | active 状态下重启后恢复最后成功画面；主动退出后重启不自动重入。 |

上表即验收清单，逐项通过才算完成硬件验收；任何本地测试结果都不能替代真实设备证据。

## 交付边界与延期范围

v0.1 仅交付本地实现、测试、授权 PW4 本地安装/验证和本地 Git 提交；不 push、不创建 GitHub Release、不发布 KindleForge、也不部署到其他设备。

本版本明确不实现浏览器/HTML/CSS 渲染、请求内容中的远程资源获取、完整 GFM（如表格）、RTL/复杂文字 shaping、局部差异刷新、智能波形、异步任务、Web 管理界面、监控、数据库、cgo 直连 FBInk、多设备抽象或云端服务。FBInk 仅作为设备上的独立可执行文件调用，未静态链接。安装时按固定 URL、版本与 SHA-256 获取字体是受完整性校验约束的安装资产流程，不是 Markdown 或显示请求的远程资源功能。

## 许可证

本项目以 **Apache-2.0** 分发，全文见 [LICENSE](LICENSE)。

随安装包分发的 FBInk 是设备上的独立可执行文件（上游 GPL-3.0-or-later），只经进程边界调用、不静态链接、不经 cgo 绑定；固定的 Noto Sans CJK SC 字体按 SIL Open Font License 1.1 分发，且不提交进本仓库。逐条依赖、版本与兼容性结论见 [docs/third-party.md](docs/third-party.md)。
