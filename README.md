# teledown-go

[`teledown`](https://github.com/hollc/teledown)（Python + Telethon）的 Go 版本，基于 [`gotd/td`](https://github.com/gotd/td)。

当前只实现 **下载** 与 **上传** 两个命令，其余功能（`refresh` / `print` / `sign` / `monit`）未移植。

## 与 Python 版的差异

### 行为差异（迁移时需要注意）

| 项 | 说明 |
|---|---|
| 会话文件 | `<账号标识>.session`：Telethon 同构的 SQLite 库（见「会话存储」一节），账号标识取 md5 的**前 16 位**（Python 版是完整 32 位，文件名不同所以两边可以同时存在）。格式与 Telethon 通用，改个名就能给 Python 版用 |
| 数字 ID 解析 | 会先查持久化的 peer 缓存，未命中才扫一次会话列表拿 `access_hash`；账号未加入该会话则无法解析 |
| `--range` 单选 | 支持 `100` 与 `=100` 两种写法；Python 版实际只认 `=100`（`100` 会被误解析成 ID 0） |
| `--prefix ""` | 视为「全部匹配」（等价 Python 的 `--prefix` 不传）；Python 版传空串会导致全部不匹配。Windows 下与 Python 的 `fnmatch` 一样**忽略大小写**（`*.MP4` 能匹配 `a.mp4`），其它平台区分大小写 |
| `--proxy` 写法 | 认 `socks5://`、`socks5h://` 与不带 scheme 的 `host:port`；**不写端口时补 1080**（`proxy.FromURL` 的默认值，与 Python 的 python-socks 一致）。Python 版走 `python_socks`，两者都支持 `user:pass@` |
| 下载顺序 | Go 版从**最新**的消息开始下载；Python 版默认 `reverse=True, min_id=1`，从**最早**的开始 |
| 视频缩略图 | **已实现**，改用 ffmpeg 抽首帧（Python 版是 moviepy + Pillow）。缩略图最大宽 320px，小于该宽度不放大；ffmpeg 缺失或抽帧失败时只提示不阻断上传 |
| 发送者过滤 | Python 版的 `-user` 参数未移植 |
| Emoji 处理 | 只覆盖常见 Unicode 区段，与 Python 的 `demoji` 全量表情表相比是近似实现 |
| 扩展名推导 | `ExtensionForMIME` 是精选表（70 余项，按 Telegram 实际会下发的 `mime_type` 收录），未收录的退回 `.bin`；Python 版走 `mimetypes.guess_extension`，表更大 |
| 视频类型判定 | 按扩展名判定（`mime.TypeByExtension`，Windows 上会读注册表）；Python 版用 libmagic 嗅探文件头，文件内容与扩展名不符时行为不同 |

### 能力增强

| 项 | 说明 |
|---|---|
| CDN 下载 | 用户账号自动开启 CDN 重定向；bot 账号自动关闭（服务端限制 bot 使用 `upload.getCdnFile`，被重定向后既取不到数也无法回退）；CDN 建连失败会自动降级到主 DC 再试一次 |
| 分片并发 | 默认 4 路并发下载，`-threads` 可调 |
| 分片校验 | `WithVerify(true)` 启用分片哈希校验，可发现 CDN 边缘节点返回的损坏数据 |
| 视频属性 | 有 `ffprobe` 时自动读取时长与分辨率，上传后支持边下边播；无 `ffprobe` 则按普通文件上传并给出提示 |
| 视频缩略图 | 有 `ffmpeg` 时抽首帧作为缩略图上传，接收端列表里能直接看到画面 |
| 进度显示 | 进行中一行原地刷新，含百分比、字节数、速度、**已用 / 剩余时间**；单个文件结束后换成一条汇总行（大小、用时、平均速度）**保留在终端里**，一批文件跑完可以回看每个文件的耗时。输出被重定向（非终端）时只留汇总行，不写控制字符 |

## 用法

复用 Python 版的 `.env`（键不区分大小写：`api_id` / `api_hash` / `phone` / `bot_token` / `save_path` / `proxy`，另支持可选的 `password`）。

```bash
# 查看当前账号登录状态（首次运行会要求输入验证码）
teledown-go info

# 下载：频道全部媒体
teledown-go download -id 1234567890

# 下载：只下 ID 大于 500 的消息中、文件名匹配 *.mp4 的
teledown-go download -id @somechannel --range ">500" --prefix "*.mp4;*.mkv"

# 下载：指定区间
teledown-go download -id 1234567890 --range "100..200"

# 上传：单个文件到收藏夹
teledown-go upload -path ./movie.mp4

# 上传：整个目录到频道，带标签，上传后删除源文件
teledown-go upload -id 1234567890 -path ./videos -at 电影 -dau Y

# 走 SOCKS5 代理
teledown-go download -id 1234567890 --proxy socks5://127.0.0.1:7890
```

### `-id` 支持的写法

Telegram 的 ID 有自己的编码约定（与 TDLib 一致），本工具按这套约定识别类型：

| 写法 | 含义 | 解析方式 |
|---|---|---|
| `-1001965235618` | 频道 / 超级群（私密频道会带 `-100` 前缀） | 直接按频道解析 |
| `1965235618` | 不带前缀的数字（通常也是频道） | 按频道 → 群 → 用户依次尝试 |
| `-1234567890` | 基础群（负号 + 群 ID） | 直接按群解析（`messages.getChats` 不需要 access_hash） |
| `@频道用户名` / `t.me/xxx` | 用户名或链接 | `contacts.resolveUsername` 等 |
| `me` | 收藏夹 | 直接使用自身 peer |

注意 `-1001965235618` 与 `1965235618` 是**同一个频道**，二者归一后是同一个 ID，共享同一条 peer 缓存。

基础群的负号形式**不带** `-100`（`-100` 专用于频道/超级群），所以两类不会混淆。ID 的归一用的是 gotd `constant.TDLibPeerID` 的算术编码（频道 = `-1000000000000 - p`），而不是剥字符串前缀——后者在 12 位频道 ID（如 `-1997852516352`）上会算错。

### info 命令

用来确认「配置对不对、账号有没有登录成功」。输出示例：

```
-----****************-----
登录方式: 手机号 +8613800138000
Name: someusername
ID: 123456789
Bot: 否
DC: 5
会话文件: 7945bd83237335e5.session
-----login successful-----
```

要点：

- 会话文件已存在时直接复用，不会重复登录；不存在时会提示输入验证码，走完流程就写成新会话。
- 首次运行若发现改造前的 `<账号标识>.gotd.json` / `<账号标识>.peers.json`，会把它们合并进会话库（原文件不删，也不再用）。
- 未设置用户名时 `Name` 回退显示「名+姓」。
- 登录失败会打印具体原因并以退出码 1 结束，方便脚本判断。
- **有 60 秒总时限**：gotd 在连不上时会持续重连，不加限制会让自检无限等待。超时提示 `操作超时（1m0s），请检查网络与代理是否可用`。`download` / `upload` 不设总时限。
- 与 Python 版的差异：Python 版每次运行都会自动打印登录信息（`main.py` 的 `show_info`），Go 版改成了显式的 `info` 命令，避免下载/上传时输出被干扰。

### 两步验证（2FA）

账号开启两步验证时，服务端在验证码之后会再要求密码。此时：

- `.env` 里配了 `password` → 直接用配置的值；
- 没配 → 提示 `该账号启用了两步验证，请输入密码（不回显）`，输入时不显示字符；
- 密码错误 → 提示 `两步验证密码不正确：请检查 .env 里 password 的值`。

登录成功后密码不需要再输（会话里已带授权）。想避免每次交互可以直接写进 `.env`（该文件已在 `.gitignore` 中）：

### 验证码与条款/注册

验证码的提示、读取与**位数校验**交给 [`gotd/contrib/auth/terminal`](https://github.com/gotd/contrib)（提示语已用 x/text catalog 换成中文）：位数与服务端声明的不符时会重新提示，不会把明显不对的验证码发出去。

条款确认与注册新账号则用同一个库的 `NoSignUp()` **明确拒绝**：

- 服务端要求接受条款 → 直接报错，不代用户接受；
- 服务端要求注册新账号（手机号写错、或该号没注册过）→ 提示 `该手机号在 Telegram 上没有可用账号…请确认 .env 里的 phone 是否正确`。

本工具只登录已有账号，不会把号码带着走注册流程。

### AUTH_RESTART

### AUTH_RESTART

服务端可能返回 `AUTH_RESTART`（要求从「发送验证码」重新走一遍），常见于上一次登录流程中断、或会话文件里残留了未完成的登录状态。处理方式与 Telethon 一致：自动重试最多 3 次（每次间隔 2 秒）。若三次都失败，会提示删除会话文件后重试：

```powershell
Remove-Item *.session   # 会话文件里只有未完成的登录状态时，删掉不影响已登录的账号
```

```ini
PASSWORD=你的两步验证密码
```

`--range` 支持的写法：

| 写法 | 含义 |
|---|---|
| `>100` | 消息 ID 大于 100 |
| `<100` | 消息 ID 小于 100 |
| `100` / `=100` | 单条消息 |
| `1,2,3` | 多条消息 |
| `10..200` / `s10s200` | 区间 |
| 省略（默认 `>0`） | 全部消息 |

### 按日期过滤

`--since` / `--until` 按消息的发送时间（本地时区）筛选，可与 `--range` 叠加使用：

```powershell
# 只下载 2026-09-01 及之后的消息
teledown-go download -id 1234567890 --since 2026-09-01

# 只下载 2026-09 整月
teledown-go download -id 1234567890 --since 2026-09-01 --until 2026-09-30
```

| 写法 | 含义 |
|---|---|
| `2026-09-01` | 按天。`--since` 从当天 00:00:00 起，`--until` 到当天 23:59:59 止（两端都含当天） |
| `2026-09-01 12:30` | 精确到分；也接受 `2026-09-01T12:30` 与 `2026/09/01` 写法 |

历史消息按时间从新到旧遍历，命中 `--since` 下界后立即停止翻页，不会为了「只看最近几个月」而读完整个频道。

### 下载目录结构

媒体按「频道 + 月份」两层归档，月份取消息的发送时间（本地时区），跨月的频道会自动分成多个月份目录：

```
<save_path>/<频道标题>-<频道ID>/<YYYY-MM>/<文件名>
# 例：./downloads/这个群-12345678/2026-08/1.jpg
```

## 依赖

| 模块 | 用途 |
|---|---|
| `github.com/gotd/td` | Telegram 客户端本体：登录、RPC、下载/上传、`peers.Storage`、`query` 迭代器 |
| `github.com/gotd/contrib` | `auth` + `auth/terminal`（验证码读取与位数校验；条款/注册用其 `NoSignUp` 拒绝）、`middleware/floodwait`（限流等待与重试）。只用到这几个包，没有引入它面向 bbolt/redis/pebble 的后端依赖 |
| `golang.org/x/net/proxy` | SOCKS5 拨号（`proxy.FromURL` 负责解析代理地址） |
| `golang.org/x/term` | 两步验证密码的不回显输入 |
| `golang.org/x/text` | 交互提示的中文词条（补在 contrib 自带的英文 catalog 上） |
| `modernc.org/sqlite` | 会话库的 SQLite 驱动。选它是因为**纯 Go、不带 cgo**，`GOOS=linux/… go build` 的交叉编译不受影响（`mattn/go-sqlite3` 需要目标平台的 C 工具链） |

刻意保留自写、不换库的部分（都核对过现成实现）：

- **扩展名映射表**：stdlib 的 `mime.ExtensionsByType` 对 Telegram 下发的 MIME 会返回空或错值（`application/x-subrip` → 空、`application/epub+zip` → `.msepub`、`video/mp4` → `.m4v .mp4 .mp4v` 需猜顺序），不如按 Telegram 的写法显式列；
- **emoji 判定**：只覆盖常见区段，与 Python 的 `demoji` 全量表情表相比是近似实现（换库需新增依赖，收益是缩小差异）；
- **进度条**：gotd 只定义 `uploader.Progress` 接口，不带终端渲染；
- **会话与 peer 持久化**：gotd 只有 `InmemoryStorage` 与 `FileSessionStorage`（一个 JSON 文件、只存 auth key 与 DC）；`contrib/storage` 是另一套抽象（面向 bbolt/pebble/redis），不实现 `peers.Storage`；
- **`--range` 语法、文件名规则、重名处理**：与 Python 版逐字对拍，没有库能保证等价（见 `parity_test.go`）。

## 构建

```bash
go build -o teledown-go.exe .

# 交叉编译（无需目标平台工具链）
GOOS=linux GOARCH=amd64 go build -o teledown-go .
GOOS=darwin GOARCH=arm64 go build -o teledown-go .
```

要求 Go 1.25 及以上（`go.mod` 固定在 1.25，依赖已钉到与之兼容的版本，不会触发工具链自动升级）。

Windows 上也可以直接用 `build.bat`，它会先把 `go vet` 跑一遍再出包：

```
build.bat                        # 默认出 windows/amd64 + linux/amd64，落在 dist\
build.bat clean nopause          # 先清 dist\ 里旧的产物，结束不按任意键
build.bat --targets=darwin/arm64,linux/arm64 --no-upx
```

要点：

- 产物名带平台后缀（`dist\teledown-go-windows-x86_64.exe`），同时写出 `dist\version.txt` 并复制一份 `.env.example`；
- 构建时强制 `CGO_ENABLED=0`（会话库用的是纯 Go 的 SQLite 驱动，交叉编译不需要 C 工具链）、`-trimpath -ldflags "-s -w"`；
- 本机装有 [UPX](https://upx.github.io/) 时自动压缩（约 27%，20MB → 5.5MB），`--no-upx` 可关掉；
- `git` 不在或没有 tag 时版本号回退成 `0.0.0` / `unknown`，不影响出包。

## 测试

```bash
go test ./...
```

覆盖范围：文件名规则、`--range` 解析、重名文件处理、通配符匹配（含 Windows 下的大小写折叠）、`.env` 解析、SOCKS5 地址解析（含不写 scheme/端口的形式）、会话库（表结构与 Telethon 逐列对拍、peer 缓存读写、带标记 ID 的编码往返、旧 JSON 迁移）、图片尺寸选择（含 `PhotoSizeProgressive`）、JPEG 校验、验证码位数校验与中文提示、上传进度的并发回调。

涉及并发的用例建议带 `-race` 跑：`go test -race ./...`。缩略图相关用例需要本机装有 ffmpeg，没装会自动跳过。

其中 `parity_test.go` 用 Python 版原函数的实际输出作为基准做跨语言对拍，确保 `shorten` 与通配符行为一致；两处刻意的行为差异在测试里注明。

## 限流（FLOOD_WAIT 等）

gotd 默认不处理 `FLOOD_WAIT`，会直接把 `rpc error code 420: FLOOD_WAIT (19)` 抛出来。而 Telegram 的等待时长会**因为立即重试而递增**（19 → 20 → …），越急越久。

等待与重试的骨架用 [`gotd/contrib/middleware/floodwait`](https://github.com/gotd/contrib) 的 `SimpleWaiter`，它比 `tgerr.AsFloodWait` 认得更全，并按 TDLib 的做法把时长夹到 `[1s, 14d]`：

| 错误 | 说明 |
|---|---|
| `FLOOD_WAIT` / `FLOOD_PREMIUM_WAIT` (420) | 同一方法的限流，本工具会打印中文提示并等满再重试 |
| `SLOWMODE_WAIT` (420) | 慢速模式群组里发言过快 |
| `2FA_CONFIRM_WAIT` (420) | 两步验证重置等待期 |
| `FLOOD_TEST_PHONE_WAIT` / `FLOOD_SKIP_FAILED_WAIT` (420) | 测试号码 / 无参数的短等待 |
| `WORKER_BUSY_TOO_LONG_RETRY` (500) | 服务端瞬时繁忙 |

策略部分（保留中文提示）在本工具里，行为与 Python 版一致：

- 遇到 `FLOOD_WAIT` / `FLOOD_PREMIUM_WAIT` 时自动等待服务端要求的时间后重试，打印 `触发限流，等待 20s 后自动重试（第 1/3 次）`；
- 单个请求最多重试 3 次；
- 要求的等待超过 10 分钟（与 Python 版的 `FLOOD_SLEEP_THRESHOLD` 一致）时不再硬等，直接报错说明需要等多久。

遇到限流时**不要连续重跑命令**——每次立即重试都会让服务端把等待时间往上加。

## 会话存储（SQLite）

会话就是**一个 Telethon 同构的 SQLite 库**（`<账号标识>.session`）：auth key、DC 与 peer 的 `access_hash` 都在里面，不再拆成两个 JSON。表结构与 Telethon 的 [`SQLiteSession`](https://github.com/LonamiWebs/Telethon/blob/master/telethon/sessions/sqlite.py) 一一对应（实现见 `tools/sessionstore.go`）：

| 表 | 用途 |
|---|---|
| `version` | 数据库版本，本工具写 `7` |
| `sessions` | auth key 与 DC，对应 gotd 的 `session.Storage` |
| `entities` | peer 的 `access_hash` / 用户名 / 手机号，对应 gotd 的 `peers.Storage`（也就是 Telethon 的实体缓存） |
| `sent_files`、`update_state` | 本工具不用，建出来只为表结构与 Telethon 一致 |
| `store_state` | 本工具自用：Telethon 没有放「联系人版本号」的地方 |

两个实现细节：

- **版本号写 7 而不是最新的 8**：Telethon 读到 `7 < CURRENT_VERSION` 会自动 `alter table` 升级，而老版本 Telethon 读到 8 会在解包 `sessions` 行时直接报错——写 7 新老版本都能用。读的时候按列名取（`SELECT dc_id, server_address, port, auth_key`），所以被 Telethon 升级过的库也照样能读。
- **`entities.id` 用 Telethon 的「带标记 peer id」**：用户 = `+id`、基础群 = `-id`、频道/超级群 = `-1000000000000 - id`（即 TDLib 的编码）。gotd 的 `peers.Key` 用 `users_` / `chats_` / `channel_` 前缀区分类型，两边在这层互转；遇到没见过的前缀直接报错，而不是往共享的库里写一个含义不明的 ID。

所以**格式与 Python 版通用**：把 `<账号标识>.session` 复制成 Python 版用的 `<完整 32 位 md5>.session` 就能接着用（账号标识的算法见 `tools/tool.go` 的 `SessionID`）。文件名仍保留 16 位前缀，是为了两边同时存在时不会互相覆盖。

首次运行会做一次迁移：发现旧的 `<账号标识>.gotd.json`（auth key + DC）与 `<账号标识>.peers.json`（access_hash、手机号映射）就合并进 `sessions` / `entities` 表，**原文件不删**，之后不再读它们。库里已有会话时不会再导入，避免旧文件盖掉更新的登录状态。

### peer 解析与缓存（`peers.Storage`）

`-id 1234567890` 这类数字 ID 必须先拿到 `access_hash` 才能调用接口。Telethon 把 entity 存进 session 的 `entities` 表，而 **gotd 的 session 只存 auth key 与 DC**，没有 peer 存储。

因此本工具实现了 gotd 的 **`peers.Storage` 接口**（与上面的会话存储是同一个对象），把 access_hash 落进 `entities` 表，解析交给 gotd 的 **`peers.Manager`**：

工作方式：

1. `Resolve` 按数字 ID 交给 `Manager.ResolveChannelID`：命中存储时只需一次 `channels.getChannels`；
2. 未命中（返回 `PeerNotFoundError`）时才扫描会话列表，并通过 `Manager.Apply` 把拿到的实体**写回存储**——这一步由 gotd 负责，会自动跳过 min 实体（无有效 access_hash 的瘦实体），避免污染缓存；
3. 之后同一个 ID 直接命中，不再碰 `messages.getDialogs`（Telegram 限流最严的接口之一）；
4. 用 `-id @用户名` 解析出的实体同样经 `Manager` 写入存储，后续用数字 ID 调用也能命中；
5. 凭据失效（`CHANNEL_INVALID` / `CHANNEL_PRIVATE`）时清除该条本地缓存并提示重新运行。

`entities` 表里只有 access_hash、手机号与联系人版本号，不含凭据；它只是缓存，删掉随时能重建（下次重新扫一遍会话列表即可）。**凭据在同一个文件的 `sessions` 表里**（256 字节 auth key），拿它就等于拿到账号，整个 `.session` 已加入 `.gitignore`，不要提交、不要外发。

扫描本身也做了收敛：命中目标立即停止翻页；翻页游标不前进时立即终止（避免重复请求同一页——这是之前的限流诱因）。

## 已知限制

- 贴纸会跳过（与 Python 版一致），但**网页预览**等其他媒体类型同样跳过。
- 上传进度与下载进度共用一个进度条实现，多文件顺序上传。
- **不支持断点续传**：下载失败或中断时会删掉 `.downloading` 临时文件，下次从 0 开始。`ResolveSavePath` 只能做「文件名 + 大小」判重，做不到「接着下」。
  gotd 的 `downloader` 没有按偏移续传的入口（`reader.offset` 从 0 起、`Builder` 也没有 `WithOffset`），`Stream` 顺序写、`Parallel` 只按绝对偏移落盘但仍从 0 请求全部数据。要做的话分片读取层不用自己写——`gotd/contrib` 的 `tg_io.ChunkSource`（内部即 `upload.getFile{Offset, Limit}` + `precise`）配 `partio.Streamer.StreamAt(ctx, skip, w)` 已经能从任意字节继续（该路径请求不带 `cdn_supported`，是主 DC 单路，也没有分片哈希校验）——代价是失去 CDN 加速、`WithVerify` 校验和 `-threads` 并发，因此暂不引入。
- 会话文件的**格式**与 Python 版通用，但没实现 `session.TelethonSession`，所以不能直接复用 Python 版的 StringSession（那是另一套纯字符串编码）。
- 上传视频依赖外部 `ffprobe` 与 `ffmpeg`：缺 `ffprobe` 时无视频属性（不可边下边播），缺 `ffmpeg` 时无缩略图，两种情况都会提示并继续上传。
- 缩略图固定取第 0 帧；Python 版同样取第 0 帧，行为一致。
- `.env` 里的 `ALIAS`（频道显示名）**尚未解析**，下载目录名一律用 Telegram 返回的频道标题。

## 目录结构

层级与 Python 版对齐，便于两版对照维护：

| Go 文件 | 对应的 Python 版 | 职责 |
|---|---|---|
| `main.go` | `main.py` | CLI 解析、命令分发、客户端生命周期 |
| `config.go` | `main.py` 的 `load_config` / `prepare_proxy` | `.env` 读取 |
| `client.go` | `main.py` 的客户端构建与登录 | 客户端、SOCKS5 拨号、登录 |
| `tools/down_file.go` | `tools/down_file.py` | 下载流程 |
| `tools/upload_file.go` | `tools/upload_file.py` | 上传流程、ffprobe 视频属性 |
| `tools/tqdm.go` | `tools/tqdm.py` | 进度显示 |
| `tools/tool.go` | `tools/tool.py`（通用部分） | 文件名规则、标题清洗、通配符、会话标识（md5 前 16 位） |
| `tools/peer.go` | `tools/tool.py`（`get_chat_id` / `get_chat_title` / `parse_user_ids`） | 频道解析（基于 gotd `peers.Manager`） |
| `tools/sessionstore.go` | Telethon 的 `sessions/sqlite.py` | 会话库：Telethon 同构的 SQLite，同时实现 gotd 的 `session.Storage` 与 `peers.Storage`，并把旧的两个 JSON 迁移进来 |
| `tools/message.go` | `tools/tool.py`（`get_history_message`） | `--range` 解析与消息遍历 |
| `tools/media.go` | `tools/tool.py`（`get_file_id` / `get_file_name`） | 媒体信息提取、file_reference 刷新 |
| `tools/thumbnail.go` | `tools/tool.py`（`get_thumb`） | 视频首帧抽图 |

说明：

- `down_file` / `upload_file` / `tqdm` / `tool` 四个文件名与 Python 版一一对应。
- Python 版把工具函数集中在 287 行的 `tools/tool.py` 里，Go 版按关注点拆成 `tool` / `peer` / `message` / `media` / `thumbnail` 五个文件，找起来更直接；每个文件顶部都标注了它对应 Python 版的哪部分。
- Python 版的 `tools/monit.py` 与 `tools/sign.py`（监控、签到）在 Go 版**尚未实现**，因此没有对应文件。
