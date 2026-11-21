# teledown 重写可行性评估

> 目的：评估未来用其他语言重写本项目的可行性。
> 核实日期：2026-09-20。所有结论均来自当日上游源码/仓库元数据实测，非二手资料。
> 当前基线：Python 3.9.13 + Telethon 1.45.0（layer 229）+ PyInstaller onefile。

## 0. 结论摘要

| | 结论 |
|---|---|
| 首选候选 | **Go + [`gotd/td`](https://github.com/gotd/td)** |
| 次选 | Rust + [`grammers`](https://codeberg.org/Lonami/grammers) |
| 不建议 | `grammers` 的 Python 绑定（`grammers` on PyPI），构造器无代理参数、无进度回调 |

决定性差异是 **CDN 下载**：grammers 至今未实现且遇 `FileCdnRedirect` 直接 panic，而 gotd/td 有完整的 CDN 状态机 + 分片哈希校验。这正是本项目 2026-09 升级 Telethon 的首要动机（1.28 里是 `raise NotImplementedError`），因此不能选一个回退该能力的方案。

第二个差异是 **会话迁移成本**：gotd/td 提供 `session.TelethonSession()`，可直接导入 Telethon 的 StringSession，切换时无需重新收短信验证码；grammers 的 `SqliteSession` 与 Telethon 格式不通，必须重新登录。

## 1. 重写面清点

现项目 927 行 Python（`main.py` 253 + `tools/*.py` 674）。

| 模块 | 职责 | 重写要点 |
|---|---|---|
| `main.py` | 7 个子命令、`.env` 读取、代理解析、会话名（md5）、命令分发 | Go 侧 `flag`/`cobra` + `godotenv`；Rust 侧 `clap` + `dotenvy` |
| `tools/down_file.py` | 下载、进度条、`.downloading` 临时文件、ref 过期重试 | 两候选都需自己接进度回调 |
| `tools/tool.py` | 频道枚举、实体解析、文件名规则、CSV 输出、通配符 | `demoji`/`fnmatch`/标准库 `csv` 需换等价物 |
| `tools/upload_file.py` | magic 判类型、ffmpeg 取首帧、上传、视频属性 | 两候选都有视频属性对应；ffmpeg 策略需重新决策 |
| `tools/sign.py` | 读 JSON 任务、发消息、按文字/行列点击 inline 按钮 | 点击按钮两候选都需走原始 API |
| `tools/monit.py` | `events.NewMessage(chats=...)` + 按发送者过滤 | 转成更新订阅 + 自行过滤 |
| `tools/tqdm.py` | 进度条 | 换库 |

### 1.1 隐式业务契约（重写最贵的部分，与语言无关）

这些规则散落在代码里，没有文档、没有测试，是重写时最易出错的地方。**建议动手前先固化成规格 + 用例**：

- Python 最终版的 `--range` 语法：`>N`、`<N`、单条 `N` 或 `=N`、闭区间 `A..B`（包含 A 和 B）、逗号分隔的消息 ID 列表。旧 `sAsB` 和 `sA..B` 写法不再支持；详见 `tools/tool.py` 的 `parse_message_range`。
- 文件名优先级：原始文件名 > 消息文本 > 文件 ID；文本经 `shorten_filename` 截断到 50 字（中间省略号），emoji 替换为 `[emoji]`
- 扩展名规则：`.jpe`/`.jpeg` 统一为 `.jpg`；非法字符 `[\\/:*?"<>|]` 剔除（标题）或替换为 `_`（文件名）
- 频道标题格式：用户为 `username(firstlast)`，频道/群为 `title`
- 落盘结构：`{save_path}/{频道标题}-{频道ID}/{文件名}`；已存在同大小文件则跳过，否则加 `(2)`、`(3)` 序号
- CSV 输出：`全部频道.csv`（列 `ID`,`频道名`，按频道名升序）与 `{标题}-{ID}.csv`（列 `链接`,`文件名`,`描述`,`大小`，按链接倒序，按「文件名+大小」去重）
- 通配符 `--prefix`：`;` 分隔的多 pattern
- `sign_tasks.json`：字段 `bot`/`action`(send|click)/`text`/`button_text`/`row`/`col`/`delay`（缺省 1.5），任务随机打乱后顺序执行

## 2. 候选对比

| 维度 | Telethon 1.45（现状） | grammers（Rust） | gotd/td（Go） |
|---|---|---|---|
| 维护活跃度 | 2026-09-10 发版 | crate 0.10.0（2026-07-02） | v0.162.0（2026-09-18），当日仍有提交 |
| 社区规模 | 生态最大 | 66 star / 25 fork | **2346 star / 210 fork** |
| 许可 | MIT | Apache-2.0 / MIT | MIT |
| CDN 下载 | 已实现（含 `CdnFileReuploadNeeded`） | **未实现，遇重定向 panic** | **完整实现 + 哈希校验** |
| 下载并发 | 无（单连接顺序） | 可自行用 tokio | `WithThreads` 多 goroutine |
| 下载校验 | 无 | 无 | `WithVerify` 分片哈希校验 |
| 代理 | SOCKS5/HTTP（`python-socks`） | 仅 SOCKS5（需 `proxy` feature） | SOCKS5 + **MTProxy** + WebSocket |
| inline 按钮点击 | `msg.click()` 现成 | 需原始 API | 需原始 API |
| 上传进度 | `progress_callback` | 需自行包 `AsyncRead` | `telegram/uploader/progress.go` 内建 |
| 复用现有会话 | — | 不兼容，需重新登录 | **可导入 Telethon StringSession** |
| 交叉编译 | 脚本 + PyInstaller | 需目标平台工具链 | `GOOS/GOARCH` 一行搞定 |
| 运行时体积 | 需 Python 运行时（onefile 打包） | 单二进制（约 5–15 MB） | 单二进制（约 10–20 MB） |

## 3. 候选 A：Rust + grammers

**具备**：`iter_dialogs`、`MessageIter`（含 `reverse`/`offset_id`）、`get_messages_by_id`、`iter_download`/`download_media`、`upload_file`/`upload_stream`、`media::Attribute::Video{duration,w,h,round_message,supports_streaming}`（与 `DocumentAttributeVideo` 一一对应）、`stream_updates`、可插拔 `RetryPolicy`（`AutoSleep` 支持 `tries`/`threshold`/`io_errors_as_flood_of`）、layer 227。

**缺口（源码位置）**：

1. **CDN 未实现且会 panic** — `grammers-client/src/client/files.rs` 请求分片时固定 `cdn_supported: false`，收到 `File::CdnRedirect` / `upload::File::CdnRedirect` 时 `panic!`（两处）。全仓库无 `GetCdnFile` 调用。
2. **无 MTProxy** — `grammers-mtsender/src/net/` 仅有 `mod.rs` 与 `tcp.rs`，唯一代理类型为 `ProxySocks5`；代理配置在 `configuration.rs` 的 `proxy_url`，且需开启 `proxy` cargo feature。
3. **按钮点击无高层封装** — 仅能读 `Message::reply_markup()`，点击需自行 `invoke` `messages.GetBotCallbackAnswer`。
4. 下载/上传进度、CSV 输出、emoji 处理、magic 判类型、视频首帧均需自行实现。

**其他风险**：README 自述未经安全审计；issue 中记录过 difference 处理相关 panic（长跑稳定性需压测）；40+ crate 依赖需用 `cargo-crev` 校验。

**优势**：与 Telethon 同作者，协议语义与命名最接近，迁移心智成本最低；Rust 类型系统可在编译期拦住 TL 参数误用。

## 4. 候选 B：Go + gotd/td

**具备**：

- `telegram/downloader/` 下 `cdn.go`、`cdn_plan.go`、`cdn_state_machine.go`、`cdn_verify.go`（9.5 KB 校验器）、`parallel.go`、`retry.go`、`web.go` → CDN 全流程 + 并发 + 校验 + 重试
- `telegram/downloader/builder.go`：`WithThreads`、`WithRetryHandler`、`WithVerify`，输出侧 `Stream(ctx, io.Writer)` / `Parallel(ctx, io.WriterAt)` / `ToPath` → 进度通过包装 writer 实现
- `telegram/uploader/`：`progress.go` 内建进度、`big.go`/`small.go` 分片策略
- 代理：MTProxy 有独立 `mtproxy/` 包（配合 `dcs.MTProxy` 生成 Resolver）；SOCKS5 需自行用 `golang.org/x/net/proxy` 拼拨号函数接进 `dcs.PlainOptions.Dial`（模块内不含 socks 实现）
- 错误与限流：`tgerr/flood_wait.go`、`telegram/flood_wait.go`
- 查询助手：`telegram/query/{messages,dialogs,channels,contacts,photos,cached,hasher}`
- 会话：`session/` 下 `storage_file.go`、`storage_mem.go`、`tdesktop.go`，以及 **`telethon.go` 的 `TelethonSession(hx string)`**
- 现成示例：`examples/save-media`、`examples/gif-download`、`examples/userbot`、`examples/takeout`、`examples/dialogs`、`examples/updates`

**缺口**：

1. **inline 按钮点击同样无 helper** — 需调原始 `tg` 层（`tg/tl_messages_get_bot_callback_answer_gen.go` 提供生成好的请求类型），比 Rust 侧顺手，但仍属自行封装。
2. **会话导入是 StringSession 而非 `.session` 文件** — 迁移时需在 Python 侧先用现有 `.session` 导出 string session，再交给 gotd。一次性动作，之后不再需要 Python。
3. 视频首帧缩略图仍需外部 ffmpeg，与现状一样（两个候选都不解决）。
4. `go 1.25` 起步，构建环境需相应版本。

**优势**：社区与维护活跃度是 grammers 的 35 倍量级；交叉编译几乎零成本，可直接替换现有 CI 的三平台矩阵；CDN/并发/校验三项免费获得，正好覆盖现状最弱的部分。

## 5. 建议路线

1. **阶段 0（现在）**：不动。继续用 Telethon 1.45.0，先把 CDN 与 flood wait 的修复实际跑一段时间。
2. **阶段 1**：固化规格。把第 1.1 节的业务契约写成文档 + 一组输入输出用例（跨语言通用），这是后续验证等价的唯一依据。
3. **阶段 2**：只读切片。用 gotd/td 实现 `refresh` 与 `print`（`iter_dialogs` + `telegram/query/messages` + CSV 输出），不碰下载。跑通即验证工具链、会话导入与 CI 可行性。
4. **阶段 3**：下载路径。`downloader` + 进度包装 + 临时文件 rename + 重试；重点验证 CDN 与非 CDN 两类文件。
5. **阶段 4**：上传 + 签到（原始 API 点按钮）。上传前先定 ffmpeg 策略。
6. **阶段 5**：`monit`（更新订阅）+ 交叉编译 CI + Release 替换。
7. **全程**：Python 版保持可用，新实现走独立分支；用同一频道双跑，比对文件名集合、数量与大小，作为切换的准入条件。

## 6. 落地进展

Go 版已落地在 `D:\Documents\Goproject\teledown-go`（go 1.25.0 + gotd/td v0.162.0），实现下载与上传，`go test ./...` 通过。

实施过程中确认到的、文档其它章节未提及的 API 细节：

- **CDN 需要显式开启**：`telegram.Options.AllowCDN` 默认 `false`（只走主 DC），必须置 `true` 才会走 CDN 重定向流程。
- **下载器选项在 Builder 上而非 Downloader 上**：`downloader.NewDownloader().Download(api, loc)` 之后才能链式 `.WithThreads(n).WithVerify(true)`。
- **`MessagesGetMessages` 直接收 ID 切片**：签名是 `(ctx, []InputMessageClass)`，不是请求结构体。
- **gotd 不处理 `FILE_REFERENCE_EXPIRED`**：下载器内部无该重试分支，需自行在失败后重新拉取消息刷新 `file_reference` 再重试（用 `tg.IsFileReferenceExpired(err)` 判定）。
- **进度需自己接**：下载用包装 `io.WriterAt`/`io.Writer` 统计字节；上传实现 `uploader.Progress` 接口的 `Chunk(ctx, ProgressState) error`。
- **数字 ID 解析需自行处理**：gotd 无 peer 缓存管理器（官方示例用 pebble 做持久化），实现里改为扫一遍会话列表用渠道 ID 匹配 `access_hash`，单次运行一次开销。
- 上传若要去掉「视频不可边下边播」的问题，需要 `DocumentAttributeVideo`；本实现改用 `ffprobe` 取时长与分辨率，未移植 Python 版的 moviepy 缩略图逻辑。

## 7. 核实方法

- 上游源码：Codeberg / GitHub 公开仓库的 master/main 分支原始文件
- 版本与元数据：crates.io、PyPI、GitHub REST API
- 本地行为：本机 Telethon 1.45.0 的解释器内省（签名、错误类、内部重试分支）
- 未核实项：两个候选的实测吞吐与长期稳定性，需在阶段 2/3 用真实账号压测
