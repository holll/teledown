# teledown

使用Telethon批量下载telegram中任意群组/频道文件

## 使用教程

1. 克隆或下载本项目代码。
2. 将 `.env.example` 复制为 `.env` 并按下文说明填写。
3. 创建并激活 Python 3 虚拟环境（可选，但推荐）。
4. 安装依赖 [教程](#jump2)。
5. 阅读命令行参数的作用 [教程](#jump3)，并按示例运行。

## 配置文件解释<a id="jump1"></a>

```
# 必填：在 https://my.telegram.org 获取后填写
API_ID=参见下方教程
API_HASH=参见下方教程

# 账号登录方式（手机号与机器人 Token 二选一填写，另一项留空）
PHONE=手机号
BOT_TOKEN=

# 下载配置
SAVE_PATH=下载文件的保存路径
PROXY=可选，形如 user:pass@ip:port 或 ip:port

# 给频道设置显示名，多个用逗号/分号/换行分隔，格式 chat_id:别名
# ALIAS=-100123456:频道A,-100987654:频道B
# 也可以写成多行（需用引号包裹）：
# ALIAS="-100123456:频道A
# -100987654:频道B"
ALIAS=-100123456:给指定频道设置显示名
```

### 获取 api_id 和 api_hash

可参考 https://www.jianshu.com/p/3d047c7516cf ，在 https://my.telegram.org 申请后填写到配置中。

## 依赖安装教程<a id="jump2"></a>

从源码运行建议使用 Python 3.12（与发布构建一致）。代码使用 Python 3.9 引入的内置泛型，不支持 Python 3.8。

1. 安装基础依赖（必需）

```bash
pip3 install -r requirements.txt
```

2. 代理参数 `--proxy` 使用基础依赖中已包含的 `python-socks`，无需另外安装 PySocks。

3. 上传功能需要的附加依赖（可选，MoviePy 仅支持 2.x）：

```bash
pip3 install -r requirements_upload.txt
# Linux 安装 python-magic
pip3 install python-magic
# Windows 安装 python-magic-bin
pip3 install python-magic-bin==0.4.14
```

4. 如果希望更快的下载速度，建议额外安装 `cryptg`（已列在 `requirements.txt`，若未自动安装请手动执行 `pip3 install cryptg`）。

## 可选参数<a id="jump3"></a>

CLI 已改为子命令模式，先选择操作类型，再填写对应参数：

```
python main.py [-c .env] [--proxy user:pass@ip:port]
               {refresh,hook,download,upload,print,monit} ...

子命令 refresh：刷新缓存，打印所有频道信息
子命令 hook：执行自定义 Hook 功能
子命令 download：
  -id     频道ID，多个频道用|或,分隔；支持 https://t.me/xxx/123 链接
  -user   指定下载的用户，支持用逗号或|分隔多个用户名/ID，默认下载所有用户
  --range 下载范围，默认">0"表示所有消息
  --since / --until 消息发送时间的下限 / 上限（本地时区，包含边界）
  --prefix 通配符，文件名前缀
子命令 upload：
  -id     频道ID，多个频道用|或,分隔
  -path   上传文件路径
  -dau    上传完成后是否删除源文件（Y/N），默认N
  -at     上传时增加的标签
子命令 print：
  -id     频道ID，多个频道用|或,分隔
子命令 monit：
  -id     频道ID，多个频道用逗号分隔
  -user   仅监控指定用户的消息，支持用逗号或|分隔多个用户，默认所有用户
  --prefix  监控时仅下载匹配通配符的文件名前缀
```

快速开始：

```bash
# 复制配置模版并填写 api_id / api_hash 以及 phone 或 bot_token（二选一）
cp .env.example .env

# 安装依赖（建议在虚拟环境内执行）
pip3 install -r requirements.txt

# 首次运行会引导输入验证码或二步验证密码
python main.py refresh
```

### 示例命令

刷新缓存并打印所有频道信息：

```bash
python main.py refresh
```

下载两个频道的全部文件，并只保留文件名前缀为 `report` 的文件：

```bash
python main.py download -id '123456789|222222222' --prefix "report*"
```

`download --range` 按频道内的**消息 ID**筛选，不是日期或条数。每个频道内按消息 ID 从小到大下载，省略 `--range` 时也一样：

| 写法 | 筛选范围 |
| --- | --- |
| `">100"` / `"<100"` | 大于 / 小于 100，不含 100；`">0"` 是默认值，表示全部 |
| `100` / `=100` | 只取消息 100 |
| `1..100` | 闭区间，按消息 ID 从小到大下载（含两端）；`100..100` 只取消息 100 |
| `100,1,5` | 只取列出的消息，按 `1 → 5 → 100` 下载 |

旧写法 `s1s100` 和 `s1..100` 已移除。若要保留旧版开区间 `1..100` 的结果，新版写 `2..99`。`<` 和 `>` 在命令行中应加引号，避免被 Shell 当成重定向。

下载频道 11111 内消息 ID 10 到 200（含两端）中符合发送者筛选的文件：

```bash
python main.py download -id 11111 -user @someone,@another --range 10..200
```

按消息发送时间下载（以运行 teledown 的机器的本地时区为准）：

```bash
python main.py download -id 11111 --since 2026-09-01 --until '2026-09-03 12:30'
```

只填日期时，`--since` 从当天 `00:00:00` 起，`--until` 到当天 `23:59:59` 止。指定 `12:30` 时边界为 `12:30:00`，也可指定秒；接受 `2026-09-01T12:30` 和 `2026/09/01`。两个参数可单独使用，也可与 `--range`、`-user` 同时使用；下载顺序仍按消息 ID 升序。时间筛选在本地逐条执行，只下载符合条件的文件，但仍可能遍历整个 ID 范围。

指定 `-user` 时，标识无法解析就报错，不会改成下载所有人的消息；发送者未知的消息也不会通过该过滤。下载结束会统计成功、跳过和失败数；单文件失败不阻止后续下载，但命令最终返回非零退出码。落盘时，频道目录名和附件文件名里的路径分隔符和其他不安全字符会被替换。

`download` 和 `monit` 会在接收文件内容前，根据 Telegram 提供的大小在保存目录所在磁盘上为临时文件预分配空间。空间不足或超出配额时，该文件不会开始下载。若系统或文件系统不支持预分配，会提示并改为检查当前剩余空间；这种检查不会锁定空间。大小未知的文件无法预分配。

上传本地目录的文件到多个频道并在完成后删除源文件：

```bash
python main.py upload -id 1111,2222 -path /data/files -dau Y -at "#归档"
```

上传前会检查文件大小。单个文件超过 Telegram 普通账号的默认上限（2,097,152,000 字节）时，仅 Premium 用户账号可继续上传。上传目录时会跳过超限文件、继续处理其他文件，最后返回非零退出码；被拒绝的文件不会被删除。

实时监控频道并在新文件出现时触发下载，并只关注指定用户的文件（多个用户用逗号分隔）：

```bash
python main.py monit -id 3333,4444 -user @someone,@another --prefix "*.pdf"
```

## 注意事项

1. 如果你想下载速度更快，`pip3 install cryptg `
2. 如果使用上传功能，需要安装额外的依赖。Linux系统安装`python-magic`、Windows系统安装`python-magic-bin==0.4.14`
3. 本程序仅支持python3

## 打包说明<a id="jump7"></a>

`build.sh` 支持两种构建变体，CI（`.github/workflows/build.yml`）会为 Linux 和 Windows 各构建一份：

| 变体 | 产物名 | 说明 | Linux amd64 本地构建参考体积 |
| --- | --- | --- | --- |
| 全功能版 | `teledown-<平台>-<架构>` | 含 `upload`；视频上传使用外部 FFmpeg | ≈41 MiB |
| 精简版 | `teledown-noupload-<平台>-<架构>` | 不含 `upload`（执行时提示不支持）；不需要 FFmpeg | ≈20 MiB |

两个变体均不内置 FFmpeg。下载、监控、普通文件上传不需要 FFmpeg；上传视频或运行全功能版自检前，请安装 FFmpeg（Debian/Ubuntu：`sudo apt install ffmpeg`；Windows：`winget install --id Gyan.FFmpeg -e`），确认 `ffmpeg -version` 可运行。也可以通过环境变量 `FFMPEG_BINARY` 指定可执行文件的完整路径。自检使用 `libx264` 生成测试视频。

Linux 全功能版内置 `libmagic` 和构建时匹配的 `magic.mgc`，识别文件类型时不依赖运行机器的规则库；源码运行仍使用系统规则。Debian/Ubuntu 上构建全功能版前，请安装 `libmagic1` 和 `libmagic-mgc`。

两个变体均用 Python 标准库导出 CSV，不依赖 pandas，且不打包 Tcl/Tk。实际体积会随平台和依赖版本变化。

本地构建：

```bash
bash build.sh                                                 # 全功能版
UPLOAD_SUPPORT=0 OUTPUT_NAME=teledown-noupload bash build.sh  # 精简版
```

可用环境变量：`UPLOAD_SUPPORT`（1/0）、`OUTPUT_NAME`、`PYTHON`、`KEEP_COMPILED`。

打包版可离线自检，无需配置或登录：全功能版执行 `teledown self-test full`，无上传版执行 `teledown-noupload self-test noupload`（用实际可执行文件名替换命令名）。全功能版会生成短视频并验证 MIME 识别和缩略图提取；无上传版会检查相关依赖是否已排除。

> Release 里的配置模板名为 `env.example`：GitHub Release 资源名不能以点开头（会被改写成 `default.env.example`），下载后 `cp env.example .env` 即可；从 Actions artifact 里取则是原始的 `dist/.env.example`。

## 常见错误

1. `Server sent a very new message with ID xxxxxxxxxxxxxxxxxxx, ignoring`
   ，如果你确定没有在多个设备上使用同一个session文件，那么这个问题是由于当前设备时间与telegram服务器时间差距过大（大于30s）。
   解决办法：服务器时间戳是`xxxxxxxxxxxxxxxxxxx >> 32`，请将本地时间戳修改到与服务器时间戳相差30s以内
2. `Could not find the input entity for XXXXXXX`，请先刷新缓存

## Star历史

<a href="https://github.com/holll/teledown/stargazers">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=holll/teledown&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=holll/teledown&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=holll/teledown&type=Date" />
  </picture>
</a>
