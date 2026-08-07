# 可复现的 release 构建

本文描述从干净检出生成唯一受支持产物（Kindle Paperwhite 4 / Linux ARMv7）的完整流程。构建成功与校验和一致只是本地构建证据，**不是** PW4 硬件验收，设备结论只能来自真实设备验收（清单见 [README](../README.md) 的「PW4 硬件验收」一节）。

## 前置条件

| 前置条件 | 说明 |
| --- | --- |
| Go 工具链 | 纯 Go、`CGO_ENABLED=0`，不需要交叉编译器或 C 工具链。 |
| `EINKRELAY_FONT_DIR` | 指向与 `assets/fonts/manifest.json` 匹配的已校验字体目录。`scripts/check.sh` 用它跑 CJK 混排黄金测试，缺失即失败，绝不把"跳过"当成通过。 |
| SHA-256 工具 | `sha256sum`、`shasum` 或 `openssl` 任一即可，脚本按此顺序探测。 |

字体二进制不入库（`.gitignore` 排除 `*.otf`/`*.ttf`/`*.ttc`），manifest 固定的是 Noto Sans CJK SC / Regular / Sans2.004，文件名 `NotoSansCJKsc-Regular.otf`。

## 执行

```sh
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  scripts/release-build.sh /absolute/output/directory
```

脚本先完整执行 `scripts/check.sh`（CJK 黄金测试 → 全量 `go test -count=1 ./...` → 临时目录 ARMv7 交叉构建），随后以下列**固定**设置构建正式产物：

```text
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7
go build -trimpath -ldflags='-s -w' -o <output>/eink-relay ./cmd/eink-relay
```

`-trimpath` 去除构建路径，使同一份源码在任意检出目录下产出相同字节；`-s -w` 去掉符号表与调试信息。四个环境变量必须逐字一致，`GOARM=7` 尤其不能省略——PW4 是 ARMv7。

## 脚本实际执行的步骤（逐步对照源码）

下面按 `scripts/release-build.sh` 的真实控制流逐步列出。这一节是为了让本文与脚本**可以逐条对照**，而不是复述意图；每一步都标注了对应的源码行。

| 步 | 动作 | 失败时 | 源码 |
| --- | --- | --- | --- |
| 1 | `set -eu`；解析出 `script_dir` 与 `repo_root`（都经 `pwd -P` 解析真实路径） | — | `:10`–`:13` |
| 2 | 校验参数：**恰好一个**且必须以 `/` 开头 | 打印两行用法到 stderr，退出 **2** | `:30`–`:34` |
| 3 | **在创建任何东西之前**拒绝仓库根 | 退出 1，不留空目录 | `:38`–`:39` |
| 4 | 按 `sha256sum` → `shasum` → `openssl` 顺序探测校验工具 | 三者皆无则退出 1 | `:44`–`:52` |
| 5 | 记录目标目录**是否由本次调用创建**，然后 `mkdir -p`，再 `pwd -P` 解析真实路径 | — | `:69`–`:72` |
| 6 | 符号链接解析**之后**再次拒绝仓库根 | 退出 1，并 `rmdir` 掉本次新建的叶子目录 | `:83`–`:84` |
| 7 | 若目标在仓库内：要求 `git check-ignore` 对 **三个**产物名（`eink-relay`、`eink-relay.sha256`、`eink-relay.buildinfo`）**全部**判定为已忽略 | 退出 1，并 `rmdir` 掉本次新建的叶子目录 | `:86`–`:95` |
| 8 | 在 `repo_root` 下执行 `sh scripts/check.sh`（CJK 黄金测试须显式 `--- PASS` → `go test -count=1 ./...` → 临时目录 ARMv7 交叉构建） | `set -e` 使整脚本失败退出 | `:100` |
| 9 | 注册 `trap cleanup EXIT HUP INT TERM`，三个产物一律先写 `.<名字>.tmp.$$` | 异常退出时清理临时文件 | `:102`–`:108` |
| 10 | 以四个固定环境变量 + `-trimpath -ldflags='-s -w'` 构建到**临时名** | 失败退出，无半成品 | `:113`–`:117` |
| 11 | `mv -f` 到最终名 `eink-relay` | — | `:121` |
| 12 | 计算 SHA-256，并校验其为 **64 位小写十六进制** | 不合格则退出 1 | `:123`–`:128` |
| 13 | 以**双空格**格式写校验和文件（先临时名再 `mv -f`） | — | `:131`–`:132` |
| 14 | **从磁盘重新读回**校验和文件与产物，二次比对（不复用内存变量） | 不一致则退出 1 | `:136`–`:139` |
| 15 | 写 `eink-relay.buildinfo`：摘要、四个目标环境变量、构建 flag、`go version`，以及**从产物读回**的 `go version -m` | — | `:150`–`:159` |
| 16 | 向 stdout 打印三行（产物路径、已验证摘要、构建记录路径） | — | `:161`–`:163` |

**关于第 8 步的顺序**：门禁跑在**归属判定之后、构建之前**。这个顺序是刻意的——先证明输出位置合法，再花几分钟跑测试，最后才写字节。因此一个非法的输出目录会**立刻**被拒绝，不会让操作者白等一遍完整测试。

**本脚本不做的事**：不 `git commit`、不 `git push`、不上传、不发布 GitHub Release、不访问网络（唯一的网络可能性来自 `go build` 的模块下载，而模块在 `go.sum` 中已固定）。全部动作都局限在 `repo_root` 的只读读取、`mktemp -d`（由 `check.sh` 负责）与目标输出目录的写入。

## 输出目录规则

脚本只接受**一个位置参数**，没有任何选项/flag。参数必须是**绝对路径**，脚本在写入任何文件之前先做归属判定：

| 目标位置 | 行为 |
| --- | --- |
| 缺参数、多参数、或相对路径 | 打印两行用法说明到 stderr（`usage: <script> <absolute-output-directory>` 与 `the directory must be outside the repository, or a path Git ignores`），退出码 **2**，不做任何构建（`release-build.sh:24-34`）。 |
| 仓库根目录本身 | 永远拒绝（退出码 1）。这一条在 `mkdir` 之前判定（`release-build.sh:38-39`），所以连空目录都不会产生；符号链接解析之后再判定一次（`release-build.sh:83-84`）。曾有一个被跟踪的构建产物打断过交付门禁，因此这里不留任何例外。 |
| 仓库内的其他路径 | 仅当 `git check-ignore` 对 `eink-relay`、`eink-relay.sha256` 和 `eink-relay.buildinfo` 三个目标路径都判定为已忽略时才允许；`git` 不可用或任一路径未被忽略时拒绝（退出码 1）（`release-build.sh:86-95`）。 |
| 仓库之外 | 允许。 |

第三行的归属判定需要解析出真实路径，而解析要求目录存在，因此脚本会先 `mkdir -p` 再判定。为了让拒绝不留痕迹，脚本记录该目录是否由本次调用创建，并在拒绝时把它 `rmdir` 掉——只删叶子目录，且仅在其为空时，所以早于本次运行存在的任何东西都不会被动到。对 `<repo>/a/b/c` 这类多层新建路径，只有叶子 `c` 会被回收。

当前 `.gitignore`（`.gitignore:3`、`:4`、`:9`、`:12`、`:17-19`）忽略根级 `/eink-relay`、`/eink-relay-*`、`/dist/`、`/.builder-*` 和字体后缀。`/dist/` 忽略的是**整个目录**，因此其中的三个产物（含 `eink-relay.buildinfo`）都随之被忽略，`dist/` 能通过上表第三行的 `git check-ignore` 判定：

```sh
EINKRELAY_FONT_DIR=/path/to/fonts scripts/release-build.sh "$PWD/dist"
```

**`dist/` 是仓库内唯一推荐的输出位置，但严格来说不是唯一能通过判定的位置。**Git 的忽略语义是：一个目录被忽略时，其下所有内容都被视为忽略，`git check-ignore` 也照此回答。因此根级 `/eink-relay`、`/eink-relay-*`、`/.builder-*` 这三条规则若被用作**目录名**（例如 `"$PWD/eink-relay-out"`），该目录下的三个产物同样会被判定为已忽略，脚本也会接受。这不会破坏仓库清洁性——产物确实不会被跟踪——但**不要这么用**：`/eink-relay` 与 `/eink-relay-*` 两条规则的本意是拦截误落在仓库根的**单个二进制**，把它们当目录用会让忽略规则的意图变得难以辨认。**请一律用 `dist/`。**

其余仓库内路径（包括 `bin/`）都未被任何规则忽略，会被脚本以退出码 1 拒绝。若不想在仓库内留产物，直接用仓库之外的目录，例如 `/tmp/einkrelay-release`。

脚本可重复执行：三个产物都先写成 `.<名字>.tmp.$$` 临时名，成功后才 `mv -f` 到最终名；异常退出由 `trap cleanup EXIT HUP INT TERM` 清理临时文件（`release-build.sh:102-108`），不会留下半成品，也不会累积旧文件。

被拒绝的调用同样不留痕迹：归属判定需要解析真实路径，脚本因此先 `mkdir -p`，并记录该目录是否由本次调用创建；拒绝时只 `rmdir` 叶子目录且仅在其为空时（`release-build.sh:69-79`）。

## 产物与校验

成功后脚本恰好产出三个文件（若输出目录本来就非空，脚本不会清理与它无关的既有文件）：

```text
<output>/eink-relay            # Linux ARMv7 可执行文件
<output>/eink-relay.sha256     # "<64位小写十六进制>  eink-relay"
<output>/eink-relay.buildinfo  # 工具链与模块版本记录，见下一节
```

并向 stdout 打印三行：

```text
release artifact: <output>/eink-relay
verified SHA-256: <64位小写十六进制>
build record:     <output>/eink-relay.buildinfo
```

脚本在返回成功前会重新读取磁盘上的两个文件并比对摘要，而不是复用内存中的变量。校验和文件采用双空格格式，`sha256sum -c` 与 `shasum -a 256 -c` 均可直接读回；校验命令必须在输出目录中运行，避免相对文件名指向源码树：

```sh
cd /absolute/output/directory

# Linux
sha256sum -c eink-relay.sha256

# macOS / 无 sha256sum 的环境
shasum -a 256 -c eink-relay.sha256

# 仅有 openssl 时（openssl 没有 -c 回读模式，只能自己比对这一行的摘要）
openssl dgst -sha256 eink-relay
cat eink-relay.sha256
```

release 产物是本地文件，不提交到 Git，也不 push、不发布 GitHub Release 或 KindleForge。

## 可复现性的真实边界（不要过度声称）

必须把"可复现"说准确，否则这份记录会被当成比它实际更强的保证：

| 条件 | 是否产出相同字节 | 依据 |
| --- | --- | --- |
| 同一 Go 工具链版本 + 同一模块版本 + 同一源码，换一个检出路径 | **是** | `-trimpath` 消除了构建目录，`CGO_ENABLED=0` 消除了 C 工具链差异，`GOOS/GOARCH/GOARM` 全部钉死 |
| 同上，换一台机器（宿主 OS/CPU 不同） | **是**（Go 的交叉编译输出只由目标三元组与工具链决定） | 同上 |
| **换一个 Go 工具链版本** | **否** | 不同版本的编译器与链接器会产出不同代码。这不是缺陷，是 Go 的既有事实 |
| 换一个 goldmark / x-image 模块版本 | **否** | 链接进来的代码变了 |

因此：**一个 SHA-256 只有在它的工具链旁边才有意义。**本仓库不声称跨工具链的 bit-for-bit 可复现，只声称"给定同一 Go 工具链版本与同一模块版本，构建是确定性的"。

为了让这句话事后可核对，脚本在产出产物之后写一份 `eink-relay.buildinfo`，内容包括摘要、四个固定的目标环境变量、构建 flag、`go version`，以及直接从产物读回的 `go version -m`（工具链版本、模块版本与 `-trimpath`/`GOARM` 等 build settings 都在其中）。这份记录是**从产物读回来的**，不是脚本按自己以为传了什么重新拼出来的，所以它反映的是真正链接进去的东西。

复核他人给出的摘要时，应当先比对 `buildinfo` 里的 `toolchain:` 一行；工具链不同则摘要不同是预期结果，不构成产物被篡改的证据。

`go.mod` 里的 `go 1.23` 是**语言版本指令，不是工具链锁**：它不会把构建固定到 1.23，实际使用的版本以 `buildinfo` 记录的为准。本仓库没有 `toolchain` 指令，也没有 CI 去钉死版本。

## 本文的执行状态

**本文描述的是 `scripts/release-build.sh` 的源码行为，截至目前尚没有任何一次文档会话实际运行过它。**撰写与复核本文的会话未执行过该脚本，因此本文选择明确标注“未执行”，而不是以读源码推断的方式伪造输出。

需要说明的是，本项目交付流水线的确定性门禁（`check` 阶段）会针对候选提交的确切字节，运行一遍完整测试套件并执行一次 ARMv7 交叉构建，作为晋升前的强制条件；因此“文档会话未曾运行 `release-build.sh`”不等于这份代码树整体未经验证——门禁验证的是测试与交叉构建本身，并不运行 `release-build.sh` 这个脚本，也不能替代本文所列的可复现构建证据。

因此上面的退出码、拒绝行为、产物清单、三行 stdout 与“脚本实际执行的步骤”一节，**全部由脚本源码逐行核对得出，尚未由任何一次真实运行确认**——连 `sh -n` 的语法检查都未能执行。

对脚本本身的改动同样**未经执行验证**：本里程碑内曾加入 `eink-relay.buildinfo` 构建记录（见上一节）；此后多个会话独立复核，逐行确认脚本已满足全部交付要求，未再改动脚本本身——四个目标环境变量逐字固定为 `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7`（`release-build.sh:115`，并原样写入构建记录 `:153`）；产物只能落在仓库之外、或落在 `git check-ignore` 证明已被忽略的仓库内路径（`:38-39`、`:83-84`、`:86-95`）；仓库根被无条件拒绝且拒绝时不留空目录（`:69-79`）；SHA-256 被计算、格式校验、写入 `eink-relay.sha256`，并**从磁盘重读回来二次比对**（`:123-139`）；全脚本没有任何 push、上传、打标签或发布动作。复核过程中同时修正了本文的文档缺陷：新增“脚本实际执行的步骤”对照表，把 `git check-ignore` 实际检查的产物数量（三个而非两个）、用法输出行数（两行）与源码重新对齐，并订正了“仓库内只有 `dist/` 能通过 `git check-ignore`”这一**不准确**的表述（见“输出目录规则”一节）。**但这仍然只是源码复核，不是执行证据**——见本节开头。

**因此本次里程碑内 `git status --porcelain` 的运行后复核也不存在**：没有执行构建，就没有产物，也就没有“运行之后”的仓库状态可查。可以确认的只有静态事实——文件系统枚举复核确认工作树中不存在 `eink-relay` 可执行文件、不存在 `dist/`、不存在 `bin/`、不存在任何 `*.otf`/`*.ttf`/`*.ttc`。**必须说清楚这条静态事实的边界**：它证明的是“至今没有构建出过产物”，而**不是**“跑完 `release-build.sh` 之后仓库仍然干净”。后者恰恰是交付门禁真正要看的那一条，而它需要一次真实运行，至今没有发生。

因此在本文档当前状态下，**不存在任何已验证的 release 构建产物**，也不得据本文声称 AC-27 / AC-10 的可复现构建已经通过。要把它变成证据，需要在具备执行能力的环境里跑一遍并记录原始输出：

```sh
# 0. 先做语法检查（本文档复核过程同样未能执行这一步）
sh -n scripts/release-build.sh; echo "exit=$?"

# 1. 真实构建
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  sh scripts/release-build.sh /tmp/einkrelay-release; echo "exit=$?"
file /tmp/einkrelay-release/eink-relay        # 应为 ELF 32-bit LSB, ARM, EABI5
wc -c < /tmp/einkrelay-release/eink-relay     # 产物字节数
(cd /tmp/einkrelay-release && shasum -a 256 -c eink-relay.sha256)
cat /tmp/einkrelay-release/eink-relay.buildinfo

# 2. 确定性：同一工具链下换一个输出目录重跑，两个摘要必须逐字相同
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  sh scripts/release-build.sh /tmp/einkrelay-release2; echo "exit=$?"
diff /tmp/einkrelay-release/eink-relay.sha256 /tmp/einkrelay-release2/eink-relay.sha256

# 3. 可重复执行：对同一目录再跑一次，事后仍应恰好是三个文件
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  sh scripts/release-build.sh /tmp/einkrelay-release; echo "exit=$?"
ls -la /tmp/einkrelay-release

# 4. 仓库清洁性：构建之后仓库必须一个字节都没多出来
#    期望——输出中不含任何构建产物，只有本里程碑预期的源码/文档改动
git status --porcelain
git status --porcelain --ignored | grep -E 'eink-relay|dist/' || echo 'no build artefact in tree'
git ls-files | grep -i 'eink-relay$' || echo 'no tracked eink-relay binary'

# 5. 仓库内合法输出位置的正例：dist/ 必须被判定为已忽略
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  sh scripts/release-build.sh "$PWD/dist"; echo "exit=$?"
git check-ignore -v dist/eink-relay dist/eink-relay.sha256 dist/eink-relay.buildinfo
git status --porcelain          # dist/ 被忽略，因此这里仍不应出现它

# 6. 拒绝路径不留痕迹的反例：bin/ 未被忽略，必须被拒绝且事后不存在
EINKRELAY_FONT_DIR=/path/to/verified/fonts \
  sh scripts/release-build.sh "$PWD/bin"; echo "exit=$?"   # 期望 exit=1
test -d bin && echo 'BUG: rejected target left a directory behind' || echo 'ok: no bin/ left'
```

第 2 步的 `diff` 只在**同一台机器同一工具链**下才必须为空；跨工具链版本比对摘要是错误的用法，理由见上一节。

第 4 步是**交付门禁真正关心的那一条**：历史上一个被跟踪的仓库根 `eink-relay` 二进制导致门禁反复自脏（提交 `80a006c`），因此“构建之后 `git status --porcelain` 不多出任何产物”必须被**实际观察**，而不是从脚本源码推断。第 6 步验证的是相反方向——被拒绝的调用连一个空目录都不许留下。**这六步至今一步都没有被执行过**，原因即本节开头所述：负责撰写与复核本文的文档会话不具备命令执行能力。

## 安装包内容

安装包的构成不是约定，而是 `scripts/install.sh` 会逐项硬检查的前置条件（`install.sh:27`–`install.sh:29`），缺任何一项都会 fail closed：

| 安装包内路径 | 来源 | 安装目标 |
| --- | --- | --- |
| `eink-relay` | 本文的 release 构建产物 | `/mnt/us/einkrelay/eink-relay`（`0755`） |
| `bin/fbink` | 单独获取的 FBInk 可执行文件，不由本仓库构建 | `/mnt/us/einkrelay/bin/fbink`（`0755`） |
| `assets/fonts/manifest.json` | 仓库内提交的固定 manifest | `/mnt/us/einkrelay/fonts/manifest.json`（`0644`） |
| `scripts/` | `install.sh`、`start.sh`、`stop.sh`、`uninstall.sh` | 不复制到设备，仅在包内执行 |

`install.sh` 从自身位置向上一级推导包根目录，所以这四个脚本必须位于包根的 `scripts/` 下。字体二进制 `NotoSansCJKsc-Regular.otf` **不在安装包里**：安装过程中由 `eink-relay fonts ensure` 按 manifest 获取并校验 SHA-256，或在设备无 CA 证书时按 README 的离线预置流程手工放入 `/mnt/us/einkrelay/fonts` 后再执行 `fonts ensure`。

在设备上从包根目录以 root 执行，参数是随包 FBInk 的公开 SHA-256：

```sh
scripts/install.sh <fbink-sha256>
```

`install.sh` 会先用该摘要跑 `eink-relay preflight`，再做字体校验，两者都不接收 Bearer 凭据。安装不写 `/etc`、不加开机钩子、不自动启动。
