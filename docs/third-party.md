# 第三方依赖

EInkRelay 的源码与本地构建产物以 [Apache-2.0](../LICENSE) 分发。本文件记录 v0.1 冻结的依赖选择、许可证及其与 Apache-2.0 再分发的兼容性结论；它不复制第三方许可证全文。所有版本号均取自仓库根目录的 `go.mod` / `go.sum`，字体条目取自 `assets/fonts/manifest.json`，不使用任何未经核对的版本或摘要。

## 依赖清单

| 组件 | 选定版本与形式 | 许可证 | Apache-2.0 再分发结论 |
| --- | --- | --- | --- |
| `github.com/yuin/goldmark` | `v1.7.8`，`go.mod` 直接依赖，静态链接进单一可执行文件；仅使用 CommonMark 核心（`cmd/eink-relay/markdown.go` 导入 `goldmark`、`goldmark/ast`、`goldmark/text`），未启用 GFM 扩展 | MIT | 宽松许可。保留其版权与许可声明后可与 Apache-2.0 项目一并分发，不对本项目施加额外条件。 |
| `golang.org/x/image` | `v0.23.0`，`go.mod` 直接依赖，静态链接；使用 `font`（`markdown.go`、`fonts.go`）、`font/opentype`（`fonts.go`）与 `math/fixed`（`markdown.go`） | BSD-3-Clause | 宽松许可。保留其版权声明与免责条款后可与 Apache-2.0 项目一并分发。 |
| `golang.org/x/text` | `v0.21.0`，`go.mod` 中标记为 `// indirect`，静态链接 | BSD-3-Clause | 由 `golang.org/x/image/font/opentype` 经 `sfnt` 字体解析路径传递引入，是构建图中真实存在的依赖（`go.sum` 同时固定其 `h1:` 与 `go.mod` 摘要），并非残留声明。宽松许可，兼容。 |
| Go 字体 `golang.org/x/image/font/gofont/{goregular,gobold,goitalic,gomono}` | 随 `golang.org/x/image v0.23.0` 提供，仅测试使用（`markdown_test.go`、`fonts_test.go`） | BSD-3-Clause | 仅用于版式与安全降级测试。仅含拉丁字形，不进入 release 产物、不安装到设备，也不构成 CJK 排版证据。 |
| evdev 输入解析 | 无第三方 evdev 库。`cmd/eink-relay/gesture.go` 以标准库 `encoding/binary`、`math/bits`、`os`、`syscall` 直接解码 Linux `struct input_event`（`gesture.go:3-15` 的 import 块中没有任何第三方包）：`eventLongSize = bits.UintSize / 8`（`gesture.go:29`）与 `inputEventSize = 2*eventLongSize + 8`（`gesture.go:33`）同时适配 32 位 ARMv7 目标与 64 位开发主机；`OpenEvdev`（`gesture.go:95`）先校验节点基名必须为 `event2`（`gesture.go:99`），再以 `O_RDONLY\|O_CLOEXEC\|O_NOFOLLOW` 只读打开（`gesture.go:112`），拒绝 `event0`/`event1`（`main.go:41`、`main.go:213`），且从不发出 `EVIOCGRAB`（`gesture.go:84`） | Go 标准库（BSD-3-Clause） | 这是计划中确定并被代码实际采用的纯 Go 标准库后备方案，未链接任何 evdev 库。它既不引入 cgo，也不引入 copyleft 静态依赖。 |
| FBInk | 设备端独立可执行文件，路径由 `EINKRELAY_FBINK_PATH` 给出（默认 `/mnt/us/einkrelay/bin/fbink`）。`cmd/eink-relay/render.go` 通过 `exec.CommandContext` 以 `-q -f -g file=<路径>` 显示（`render.go:51`）、以 `--help` 探测并取其首行版本横幅（`render.go:75`；上游 FBInk 没有 `--version` 选项）；`main.go:973-1001` 的 `CheckPreflight` 也只是对这个外部文件做 stat、可选 SHA-256 校验并执行 `--help`（安装期由 `install.sh:11,19-25,46` 强制传入该摘要），全程没有任何链接关系 | GPL-3.0-or-later（上游） | 仅以独立进程调用：不编译进本项目、不静态链接、不经 cgo 绑定，仓库中不含其源码或目标码（安装包中的 `bin/fbink` 由操作者提供并附带其公开 SHA-256）。EInkRelay 与它之间只有进程边界与命令行接口，因此 EInkRelay 不构成其衍生作品，可继续以 Apache-2.0 分发；随安装包分发该可执行文件时，它仍按自身 GPL-3.0-or-later 条款单独分发，需一并保留其许可证文本与源码获取途径。 |
| Noto Sans CJK SC Regular | 版本 `Sans2.004`，文件 `NotoSansCJKsc-Regular.otf`，`16437364` 字节，SHA-256 `2c76254f6fc379fddfce0a7e84fb5385bb135d3e399294f6eeb6680d0365b74b`，来源 `https://raw.githubusercontent.com/notofonts/noto-cjk/Sans2.004/Sans/OTF/SimplifiedChinese/NotoSansCJKsc-Regular.otf` | SIL Open Font License 1.1（`https://raw.githubusercontent.com/notofonts/noto-cjk/Sans2.004/LICENSE`） | OFL-1.1 与本程序的 Apache-2.0 分发兼容：字体是未经修改的独立作品，以数据文件形式加载，不参与链接。**仓库不提交该二进制**，`assets/fonts/manifest.json` 是其再分发记录；安装步骤按上述规范 URL 获取并校验固定 SHA-256 后才投入使用，运行期加载器再次校验摘要并在不匹配时 fail closed。manifest 目前只固定这一个文件，粗体、斜体与等宽角色均回落到它；新增字体文件须补齐同样的固定记录（URL、版本、字节数、SHA-256、许可证）。 |

## 静态链接与 copyleft 结论

被静态链接进 `eink-relay` 的第三方代码只有 `github.com/yuin/goldmark`（MIT）、`golang.org/x/image`（BSD-3-Clause）、`golang.org/x/text`（BSD-3-Clause）以及 Go 标准库，全部为宽松许可。**本项目未引入任何需要静态链接的 copyleft 依赖**：唯一的 copyleft 组件 FBInk 只作为设备上的独立可执行文件被调用。构建以 `CGO_ENABLED=0` 完成，源码中没有 `import "C"`，因此也不存在通过 cgo 与原生库链接的情况。

模块图由 `go.mod` 与 `go.sum` 固定，校验和取自官方 Go module 代理；没有 `replace` 指令、没有 `vendor/` 目录、没有手写的 `h1:` 摘要。`go.mod` 的三条 require（`github.com/yuin/goldmark v1.7.8`、`golang.org/x/image v0.23.0`、`golang.org/x/text v0.21.0 // indirect`）与上表逐条对应，无遗漏项，也没有列出 `go.mod` 中不存在的模块。

分发链接进来的宽松依赖或单独获取的字体时，须在本项目 Apache-2.0 许可证之外保留它们各自适用的上游版权与许可声明。

## 项目许可证一致性

| 位置 | 声明 |
| --- | --- |
| `LICENSE` | Apache License 2.0 全文（含全部 9 节条款、`END OF TERMS AND CONDITIONS` 与标准附录模板） |
| `README.md:5`、`README.md:241`（章节标题 `## 许可证` 在 `README.md:239`） | 「许可证为 [Apache-2.0](LICENSE)」「本项目以 **Apache-2.0** 分发，全文见 [LICENSE](LICENSE)」；`README.md:243` 另行复述 FBInk（GPL-3.0-or-later，独立进程）与字体（OFL-1.1，不入库）的结论，与本文件一致 |
| `docs/openapi.yaml:10-12` | `info.license`：`name: Apache License 2.0`、`identifier: Apache-2.0` |

三处声明一致，均指向同一 Apache-2.0 许可证。

`LICENSE` 的逐字核对结果：共 **202 行**，即 `https://www.apache.org/licenses/LICENSE-2.0.txt` 的形态（首行为空行，其后 201 行为规范正文）；第 2-4 行是 `Apache License` / `Version 2.0, January 2004` / 许可证 URL 三行标题；9 节条款齐全且顺序正确（`1. Definitions.` 第 8 行、`2.` 第 67 行、`3.` 第 74 行、`4.` 第 90 行、`5.` 第 131 行、`6.` 第 139 行、`7.` 第 144 行、`8.` 第 154 行、`9.` 第 166 行）；第 177 行 `END OF TERMS AND CONDITIONS`，第 179 行起为 `APPENDIX`，第 190 行保留未填写的 `Copyright [yyyy] [name of copyright owner]` 模板，末行为 `limitations under the License.`。反向核对：grep `MIT License`、`GNU General Public`、`GPL`、`Permission is hereby granted, free of charge`、`BSD` 在 `LICENSE` 中**零命中**，未混入任何其它许可证文本；文件为纯 ASCII、仅 LF、无行尾空白。

需要如实说明一处**方法上的局限**：本次核对在无 shell、且无法读取仓库外路径的环境中完成，因此没有对 `LICENSE` 做过 SHA-256 计算，也没有与本地权威副本（如 Go module 缓存中某个 Apache-2.0 模块的 `LICENSE`）做逐字节 `diff`。上述结论来自整篇文本的逐行阅读与反向 grep。具备 shell 的复核者可用一条命令补齐这道证据：`diff LICENSE <本地任一 Apache-2.0 模块的 LICENSE>`（若该副本无首行空行，则唯一差异应是第 1 行）。
