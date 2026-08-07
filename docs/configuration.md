# 运行配置

EInkRelay 在打开监听器之前读取本地环境变量。配置无效时启动即失败，不会接受任何 HTTP 请求（`cmd/eink-relay/main.go:159`、`main.go:204`）。

本文所有默认值均逐条对照 `cmd/eink-relay/` 源码核验，括号内为定义位置。

## 环境变量

| 变量 | 默认值 | 定义位置 | 说明 |
| --- | --- | --- | --- |
| `EINKRELAY_LISTEN_ADDRESS` | `0.0.0.0` | `main.go:84` | USBNetwork 与 Wi-Fi 共用的单一监听地址。 |
| `EINKRELAY_LISTEN_PORT` | `8080` | `main.go:85` | 监听端口，取值范围 1–65535。 |
| `EINKRELAY_STATE_DIR` | `/var/local/einkrelay` | `main.go:35`、`main.go:86` | 持久状态目录。设置它会同时重定位 token、Guardian socket 与 activity 记录的默认路径（`main.go:114`）；下面三个变量仍可单独覆盖。 |
| `EINKRELAY_TOKEN_PATH` | `<state dir>/token` | `main.go:87` | 本地 Bearer token 文件，必须是普通文件、非符号链接、权限恰好 `0600`、长度 32–512 字节，且每个字节都在可打印 ASCII 范围 `0x21`–`0x7e` 内（`main.go:511`）。 |
| `EINKRELAY_GUARDIAN_SOCKET` | `<state dir>/guardian.sock` | `main.go:88` | Guardian 控制 socket；`POST /v1/system/exit` 经由它请求退出（`main.go:1130`）。 |
| `EINKRELAY_ACTIVITY_PATH` | `<state dir>/activity.json` | `main.go:89` | 独占模式与主动退出的持久意图记录。 |
| `EINKRELAY_INPUT_DEVICE` | `/dev/input/event2` | `main.go:90` | 只读触摸输入节点。`event0`/`event1` 在配置校验与打开两处均被拒绝（`main.go:41`、`main.go:213`、`gesture.go:98`），电源键路径永不被打开、抓取或重映射。打开器还要求基名恰为 `event2`，并以 `O_NOFOLLOW` 拒绝符号链接、非字符设备以及 `Rdev` 在 `Lstat` 与打开之间发生变化的情况（`gesture.go:95-161`）；因此把本变量指向其他节点不会读到那个节点，只会让手势监视器持续重试重开（间隔 250 毫秒起、2 倍退避、5 秒封顶）并记录一条收敛后的日志（`gesture.go:695`、`gesture.go:695`），Guardian 监督与 REST 退出路径不受影响。 |
| `EINKRELAY_FBINK_PATH` | `/mnt/us/einkrelay/bin/fbink` | `main.go:91` | 外部 FBInk 可执行文件，由 `preflight` 校验并作为独立进程调用。 |
| `EINKRELAY_IMAGE_MAX_BYTES` | `10485760`（10 MiB） | `main.go:33` | 图片请求编码体积上限。 |
| `EINKRELAY_MARKDOWN_MAX_BYTES` | `1048576`（1 MiB） | `main.go:34` | Markdown 请求体积上限。 |
| `EINKRELAY_IMAGE_MAX_DIMENSION` | `8192` | `image.go:64`（另见 `main.go:93`） | 声明图像单边像素上限，解码前检查。 |
| `EINKRELAY_IMAGE_MAX_PIXELS` | `32000000` | `image.go:64`（另见 `main.go:95`） | 声明图像像素总数上限，解码前检查。 |
| `EINKRELAY_IMAGE_MAX_DECODED_BYTES` | `50331648`（48 MiB） | `image.go:27` | 解码前的最坏情况内存预算，由声明的头部估算。像素数并不约束字节数：同时满足上面两项的图像仍可能要求标准库分配数百 MB，在约 490MB 内存的 ARMv7 设备上那是致命的 OOM 退出而不是一个错误响应。**取值来自设备实测**：12.2 MP 的 JPEG（估算 49 MB）实测峰值 76 MB，`MemAvailable` 由 167 MB 降到约 111 MB。比最初的 64 MiB 紧，理由是独占模式重设计后原生界面不再被停掉、峰值余量不再由本进程独占；**不是**先前误记的"设备只剩 10 MB"——那是 `free` 列的误读，它不含可回收缓存。估算模型本身是准的（真实峰值约为估算的 1.15 倍）。 |
| `EINKRELAY_FONT_DIR` | `/mnt/us/einkrelay/fonts` | `fonts.go:34`、`fonts.go:163` | 存放 `manifest.json` 与固定字体文件的目录，必须是绝对路径（`fonts.go:175`）。它与其他大体积运行资源一起位于 `/mnt/us`，不放在根分区。 |
| `EINKRELAY_FONT_MANIFEST` | `<font dir>/manifest.json` | `fonts.go:163`、`fonts.go:172` | 仅覆盖 manifest 位置，同样必须是绝对路径。 |
| `EINKRELAY_READ_TIMEOUT` | `15s` | `main.go:96` | 同时用作 `ReadHeaderTimeout` 与 `ReadTimeout`（`main.go:967-968`）。 |
| `EINKRELAY_TRANSACTION_TIMEOUT` | `60s` | `main.go:97` | 同步显示事务的总期限（`main.go:759`）。 |
| `EINKRELAY_GESTURE_TAP_WINDOW` | `1s` | `main.go:98` | 四角退出手势的连点时间窗：同一角落 3 次点按必须在此时长内完成；识别器自身的兜底值同为 1s（`gesture.go` 的 `NewGestureRecognizer`）。 |
| `EINKRELAY_LIFECYCLE_TIMEOUT` | `10s` | `main.go:98` | 进入与离开独占模式的期限，同时用作 Guardian 客户端/服务端的超时（`main.go:1130`、`guardian.go:486`）。两个方向都只是一次 lipc 属性写入加一次面板帧的存/取，不再有原生 UI 单元重启，因此旧的 45s（为约 25 秒的单元重启加重绘留的余量）已经不描述设备上发生的任何事。PW4 实测：REST 退出端到端约 **0.3 秒**。 |

取值格式：端口与字节/像素类为十进制整数（`main.go:165`、`main.go:178`），超时类为 Go `time.ParseDuration` 语法，如 `15s`、`1m`（`main.go:191`）。空字符串等同于未设置，保留默认值。

上表即代码中读取的全部环境变量，共 19 个（`main.go` 16 个、`fonts.go` 2 个、`image.go` 独有 1 个），没有未文档化的变量，也没有已文档化但代码不读取的变量。

## 脚本与自定义路径

安装、启动、停止与卸载脚本有意固定管理 `/mnt/us/einkrelay` 和 `/var/local/einkrelay`。这避免安装收据或恢复脚本把调用者提供的环境值变成删除或恢复目标。脚本会继承监听、渲染和超时等环境变量，但不以 `EINKRELAY_STATE_DIR`、`EINKRELAY_TOKEN_PATH`、`EINKRELAY_GUARDIAN_SOCKET` 或 `EINKRELAY_ACTIVITY_PATH` 改写自己的 token 检查、Guardian socket 或安装收据位置。

因此，标准 Kindle 安装应使用默认路径并通过四个脚本管理。若开发或受控部署确实需要重定位持久状态，必须在同一个受控环境中直接先运行 `eink-relay resume`、再运行 `eink-relay guard`，并自行承担该非标准布局的安装、恢复与卸载操作；不要把默认脚本与自定义状态路径混用。

`EINKRELAY_IMAGE_MAX_DIMENSION` 与 `EINKRELAY_IMAGE_MAX_PIXELS` 被解析两次：一次进入 `Config`（`main.go:141`、`main.go:144`，仅参与启动校验），一次进入 `ImageLimits`（`image.go:69`、`image.go:72`），真正的请求判定使用后者（`image.go:283`）。两处默认值相同，覆盖时也取同一个值，因此外部行为一致。

`MaxHeaderBytes` 固定为 16 KiB，不可配置（`main.go:969`）。

`scripts/install.sh` 生成 token 时从 `/dev/urandom` 取 32 字节熵、编码成 64 个十六进制字符并**不写尾随换行**（`install.sh:58-64`），随后 `chmod 0600`（`install.sh:66`）；`scripts/start.sh` 每次启动前再确认它不是符号链接并重置为 `0600`（`start.sh:18-19`）。这正是 `LoadToken` 接受的形状：多一个换行就会因不可打印字节而校验失败。

## 不可配置的固定常量

以下阈值没有对应环境变量，硬编码在实现中：

| 常量 | 值 | 定义位置 |
| --- | --- | --- |
| 顶角热区边长 | 屏幕短边的 15%（最小 1 像素） | `gesture.go:231` |
| 热区数量与位置 | 左上、右上各一个正方形 | `gesture.go:235` |
| failsafe 失败次数 | 5 | `guardian.go:358` |
| failsafe 统计窗口 | 60 秒（`time.Minute`） | `guardian.go:351` |
| “启动成功”判定门槛 | 服务存活满 10 秒才不计入失败 | `guardian.go:344` |
| 重启退避序列 | 1s、2s、4s、8s（此后重复 8s） | `guardian.go:312` |
| 子进程 SIGTERM 后的宽限期 | 5 秒 | `guardian.go:288` |
| FBInk 版本探测超时 | 5 秒 | `render.go:22` |
| 回滚重显示超时 | 15 秒 | `persist.go:33` |
| 帧缓冲几何上限 | 8192 | `screen.go:37` |
| 单个字体文件上限 | 48 MiB | `fonts.go:42` |
| token 长度范围 | 32–512 字节（`main.go:513`、`main.go:517`），且每字节均为 `0x21`–`0x7e`（`main.go:520-524`） | `main.go:511-526` |
| 手势轮询间隔 | 100 毫秒（手指静止不产生事件时按时钟复核） | `gesture.go:532` |
| 手势重开退避 | 250 毫秒起，每次 ×2，5 秒封顶；重开成功后归零 | `gesture.go:551-552`、`gesture.go:563-572`、`gesture.go:695` |
| 手势失败日志汇总间隔 | 5 分钟（首次失败、原因变化与恢复始终立即记录） | `gesture.go:558`、`gesture.go:576-616` |
| 连续读失败后重开触摸节点 | 3 次 | `gesture.go:561` |
| Guardian 控制协议单行上限 | 64 字节 | `guardian.go:49` |
| HTTP 优雅关闭期限 | 5 秒 | `main.go:1143` |
| 被处理的信号 | 仅 SIGINT 与 SIGTERM；SIGHUP 不在其中，关闭终端会在不把面板还回去的情况下杀掉 Guardian | `main.go:1163` |

## 鉴权与请求优先级

没有匿名端点（`/healthz` 已于 2026-08-06 从契约移除，与未知路径一样 404）。`/v1` 下的每条路径都先鉴权，然后才做方法检查、取锁、参数校验、媒体类型校验和请求体读取（`main.go:567`）。显示端点随后尝试一个共享的非阻塞事务锁；忙时立即返回 `409 display_busy`，不读取请求体、不排队、不创建后台任务（`main.go:611`、`main.go:617`）。

方法分流按 `docs/openapi.yaml` 为每个操作声明的响应集执行，不是统一的 405：

- `GET /v1/status`（`main.go:574`）、`POST /v1/system/exit`（`main.go:598`）声明了 405，因此非法方法得到 `405 method_not_allowed`。
- `PUT /v1/display/image`、`PUT /v1/display/markdown` 在冻结契约中没有声明 405（`docs/openapi.yaml:90-110`、`124-144`），因此非 PUT 请求得到 `404 not_found`，与未知路径同一分支（`main.go:593-604`、`main.go:612`）。

因此实际优先级是：`401` → 方法分流（`405` 或 `404`） → `409` → 请求校验类错误。

两条与状态码相关的补充规则：

- 所有返回 `Status` 文档的分支（`GET /v1/status`、两个显示端点的 200、`POST /v1/system/exit`）在写状态行之前先按冻结的 `Status` schema 自检快照并序列化到缓冲区；不合规则改为契约已声明的 `500 internal_error`（`main.go:877-924`）。这四个操作本来就都声明了 500，因此没有引入未声明的响应。
- Markdown 请求体长度为 0 时以 `400 invalid_request` 拒绝，位置在 UTF-8 校验之前、任何渲染与提交之前（`main.go:688-691`）。

## 图片守卫

PNG 与 JPEG 请求在把负载交给解码器之前，先从声明的头部检查（`image.go:98`、`image.go:163`）。任一边长超过 `EINKRELAY_IMAGE_MAX_DIMENSION`、像素总数超过 `EINKRELAY_IMAGE_MAX_PIXELS`、或估算解码预算超过 `EINKRELAY_IMAGE_MAX_DECODED_BYTES` 时，返回 `413 image_dimensions_exceeded`（`image.go:283`、`main.go:796`）。

渐进 JPEG（SOF2，`image.go:210`）、CMYK/YCCK JPEG（四分量或 Adobe APP14 transform 为 2，`image.go:250`）与隔行 PNG（`image.go:145`）以相同状态码与错误码拒绝，因为它们都会把解码器的工作集放大到远超像素数所暗示的规模。声明 Adobe APP14 transform 为 0、或帧头分量标识为 `R`、`G`、`B` 的三分量 JPEG 按每像素 8 字节而非 4 字节计入预算（`image.go:277`），因为标准库会把它当作已经是 RGB 并额外分配一整幅 RGBA。这类图像因此在约一半像素数处即被拒绝；估算刻意偏保守，同时带 JFIF APP0 的 JPEG 也按同样方式计算，尽管该标记本会抑制那次额外分配。

Adobe APP14 段可以出现在帧头之后，标准库采用它见到的最后一个 transform，因此头部会一直走到 SOS 或 EOI 才作判定（`image.go:163`）。

被拒绝的请求不会调用显示后端、不会清屏，也不会改动 `current.png` 或 `previous.png`。

## 字体

`assets/fonts/manifest.json` 是随包字体的提交记录：family、version、规范 HTTPS URL、字节数、SHA-256、安装文件名与许可证。字体二进制从不提交进仓库。

安装会把该 manifest 复制进 `EINKRELAY_FONT_DIR`，随后运行 `eink-relay fonts ensure`：已匹配固定摘要的文件原样复用；否则从 manifest 固定的 HTTPS URL 下载到同目录的临时文件，边写边校验 SHA-256，fsync 后原子重命名并 fsync 目录（`fonts.go:297`、`fonts.go:327`）。任何失败都会删除临时文件并以非零状态退出，绝不留下半安装的字体。该下载仅发生在显式的字体安装/修复命令中；`serve`、图片请求和 Markdown 请求都不会联网。

设备缺少可用 CA 证书时下载步骤会失败。由于完整性由固定摘要保证，可以在联网主机按 manifest 中的 URL 取回文件、用 `sha256sum` 核对后放到目标路径，再运行 `fonts ensure` 就地校验并复用。摘要要求永不放宽。

`serve` 在启动时校验每个固定字体（`main.go:1099`）。缺失或不匹配会禁用 Markdown 渲染，并作为持久错误经 `/v1/status` 上报（`main.go:1120`）；它既不会退化成一页缺字符方块，也不会停掉服务，`/v1/status` 与 `/v1/system/exit` 在修复期间保持可达。

manifest 当前只固定一个文件，bold、italic、mono 三个角色都回退到 regular（`fonts.go:458`）。

## 显示事务与启动恢复

`serve` 在请求时从帧缓冲探测面板几何，而不是编译进固定分辨率（`screen.go:44`）。一次显示请求会被渲染、写入 `EINKRELAY_STATE_DIR` 下的候选文件、校验为与屏幕等大的灰度 PNG、fsync，然后才交给 FBInk（`persist.go:79`）。面板从不预先清屏。只有 FBInk 成功之后，`current.png` 才轮换为 `previous.png`、候选文件才被重命名就位，随后 fsync 目录。

当持久 activity 记录表明设备处于独占模式时，`serve` 在启动时重新校验并重新显示 `current.png`，失效时回退 `previous.png`（`render.go:197`）。两者都无法校验时，面板保持原样，并把该情况经 `/v1/status` 上报；未经校验的数据永不显示。

本地测试与 ARMv7 交叉构建不是硬件验收。本文所有数值均来自源码静态核对，未在任何 Kindle 上执行；设备行为以 README 的「PW4 硬件验收」清单逐项验证为准。
