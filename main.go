// teledown-go 是 teledown 的 Go 版本，基于 gotd/td，目前只实现下载与上传。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"

	"teledown-go/tools"
)

// infoTimeout 限制 info 命令的连接与登录耗时：代理不可达时 gotd 会持续重连，
// 不加时限会让「快速自检」变成无限等待。
const (
	infoTimeout = 60 * time.Second
	qrTimeout   = 3 * time.Minute
)

var errInterrupted = errors.New("收到退出信号，操作已中止。")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	command := os.Args[1]
	args := os.Args[2:]

	var err error
	switch command {
	case "info":
		err = runInfoCommand(args)
	case "download":
		err = runDownloadCommand(args)
	case "upload":
		err = runUploadCommand(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", command)
		usage()
		os.Exit(2)
	}

	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if errors.Is(err, errInterrupted) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `teledown-go - 基于 gotd/td 的 Telegram 下载上传工具

用法:
  teledown-go info [--qr]                          查看账号信息，可用手机扫码登录
  teledown-go download -id <频道ID|@用户名> [--range <范围>] [--prefix <通配符>] [--since <日期>] [--until <日期>]
  teledown-go upload   -id <频道ID|@用户名|me> -path <文件或目录> [-dau Y] [-at <标签>]

通用参数（放在子命令之后）:
  -c <路径>     配置文件，默认 .env
  --proxy <地址>  SOCKS5 代理，格式 socks5://user:pass@host:port 或 host:port

download / upload 额外参数:
  -threads <n>  分片并发上限：下载默认 4，Premium 默认 8；上传默认 4
  --transfer-stats  在 stderr 输出传输诊断
  --resume          仅 download：启用完整文档的校验续传

--range 支持的写法（按消息 ID 从小到大下载）:
  >100        消息 ID 大于 100
  <100        消息 ID 小于 100
  100         单条消息
  1,2,3       多条消息
  10..200     闭区间，包含两个端点

--since / --until 支持的写法（按消息发送时间过滤，本地时区）:
  2026-09-01        按天，含当天整天（--since 从 00:00:00，--until 到 23:59:59）
  2026-09-01 12:30  精确到分（也接受 2026-09-01T12:30、2026/09/01）
`)
}

// commonFlags 注册所有子命令共用的参数。
func commonFlags(fs *flag.FlagSet) (config *string, proxy *string) {
	config = fs.String("c", ".env", "配置文件路径")
	proxy = fs.String("proxy", "", "SOCKS5 代理地址")
	return
}

// parseCommandFlags rejects positional arguments instead of silently ignoring them
// and any flags following them.
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("不支持位置参数: %q", fs.Arg(0))
	}
	return nil
}

// runInfoCommand 只做登录与账号信息展示，供确认登录是否成功。
func runInfoCommand(args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	config, proxyAddr := commonFlags(fs)
	scanQR := fs.Bool("qr", false, "在终端显示登录二维码（仅用户账号）")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}

	// 保留快速自检的时限，扫码时给手机确认和两步验证留出时间。
	timeout := infoTimeout
	if *scanQR {
		timeout = qrTimeout
	}
	return withClient(*config, *proxyAddr, timeout, *scanQR, func(ctx context.Context, client *telegram.Client, cfg *Config, _ *tools.SessionStore) error {
		return showInfo(ctx, client, cfg, *scanQR)
	})
}

// showInfo 打印账号信息，对应 Python 版 main.py 的 show_info。
func showInfo(ctx context.Context, client *telegram.Client, cfg *Config, scanQR bool) error {
	me, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("获取账号信息失败: %w", err)
	}

	loginMethod := "手机号 " + cfg.Phone
	if cfg.BotToken != "" {
		loginMethod = "机器人 Token"
	} else if scanQR {
		loginMethod = "扫码模式（有会话时直接复用）"
	}

	name := me.Username
	if name == "" {
		name = strings.TrimSpace(me.FirstName + " " + me.LastName)
	}
	if name == "" {
		name = "(未设置)"
	}

	fmt.Println("-----****************-----")
	fmt.Printf("登录方式: %s\n", loginMethod)
	fmt.Printf("Name: %s\n", name)
	fmt.Printf("ID: %d\n", me.ID)
	fmt.Printf("Bot: %s\n", yesNo(me.Bot))
	fmt.Printf("DC: %d\n", client.Config().ThisDC)
	fmt.Printf("会话文件: %s\n", cfg.SessionPath())
	fmt.Println("-----login successful-----")
	return nil
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func runDownloadCommand(args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	config, proxyAddr := commonFlags(fs)
	threads := fs.Int("threads", tools.DefaultThreads, "分片并发数")
	transferStats := fs.Bool("transfer-stats", false, "在 stderr 输出传输诊断")
	channelID := fs.String("id", "", "频道 ID 或 @用户名")
	rangeArg := fs.String("range", ">0", "消息范围")
	prefix := fs.String("prefix", "", "文件名通配符，; 分隔多条")
	resume := fs.Bool("resume", false, "启用完整文档的校验续传")
	since := fs.String("since", "", "只下载该日期（含）之后的消息，如 2026-09-01")
	until := fs.String("until", "", "只下载该日期（含）之前的消息")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if *channelID = strings.TrimSpace(*channelID); *channelID == "" {
		return fmt.Errorf("必须指定 -id")
	}
	if *threads <= 0 {
		return fmt.Errorf("-threads 必须大于 0")
	}

	msgRange, err := tools.ParseRange(*rangeArg)
	if err != nil {
		return err
	}
	dateRange, err := tools.ParseDateRange(*since, *until)
	if err != nil {
		return err
	}

	threadsExplicit := false
	fs.Visit(func(f *flag.Flag) { threadsExplicit = threadsExplicit || f.Name == "threads" })
	var stats *tools.TransferStats
	if *transferStats {
		stats = new(tools.TransferStats)
		defer stats.Report(os.Stderr)
	}
	return withClient(*config, *proxyAddr, 0, false, func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error {
		ctx = tools.WithTransferStats(ctx, stats)
		workerLimit, profileErr := downloadThreadLimit(ctx, *threads, threadsExplicit, cfg.BotToken != "", func(ctx context.Context) (bool, error) {
			me, err := client.Self(ctx)
			if err != nil {
				return false, err
			}
			return me.Premium && !me.Bot, nil
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if profileErr != nil {
			fmt.Fprintf(os.Stderr, "无法确认 Premium 状态，下载使用普通并发上限 %d\n", workerLimit)
		}
		manager := tools.NewPeerManager(client.API(), store)
		peer, err := manager.Resolve(ctx, *channelID)
		if err != nil {
			return err
		}
		fmt.Printf("开始下载 %s\n", peer.Title)
		return tools.Download(ctx, client, peer, tools.DownloadOptions{
			Range:    msgRange,
			Date:     dateRange,
			Prefix:   *prefix,
			SavePath: cfg.SavePath,
			Threads:  workerLimit,
			Manager:  manager,
			Resume:   *resume,
			Stats:    stats,
		})
	})
}

func runUploadCommand(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	config, proxyAddr := commonFlags(fs)
	threads := fs.Int("threads", tools.DefaultThreads, "分片并发数")
	transferStats := fs.Bool("transfer-stats", false, "在 stderr 输出传输诊断")
	channelID := fs.String("id", "", "频道 ID、@用户名或 me")
	path := fs.String("path", "", "要上传的文件或目录")
	deleteAfter := fs.String("dau", "N", "上传后是否删除源文件 (Y/N)")
	tag := fs.String("at", "", "追加到描述前的标签，不带 #")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("必须指定 -path")
	}
	if *channelID = strings.TrimSpace(*channelID); *channelID == "" {
		*channelID = "me"
	}
	if *threads <= 0 {
		return fmt.Errorf("-threads 必须大于 0")
	}
	var deleteFlag bool
	switch strings.ToUpper(strings.TrimSpace(*deleteAfter)) {
	case "Y":
		deleteFlag = true
	case "N":
	default:
		return fmt.Errorf("-dau 只能为 Y 或 N")
	}

	var stats *tools.TransferStats
	if *transferStats {
		stats = new(tools.TransferStats)
		defer stats.Report(os.Stderr)
	}
	return withClient(*config, *proxyAddr, 0, false, func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error {
		ctx = tools.WithTransferStats(ctx, stats)
		peer, err := tools.NewPeerManager(client.API(), store).Resolve(ctx, *channelID)
		if err != nil {
			return err
		}
		me, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("获取当前账号的 Premium 状态失败: %w", err)
		}
		return tools.Upload(ctx, client.API(), peer, tools.UploadOptions{
			Premium:     me.Premium && !me.Bot,
			Path:        *path,
			DeleteAfter: deleteFlag,
			Tag:         *tag,
			Threads:     *threads,
		})
	})
}

// withClient 负责读配置、开会话库、建客户端、登录，并在登录后执行 fn。
// timeout 大于 0 时给整个连接+登录+执行过程加时限，0 表示不限制（下载/上传不应有总时限）。
func withClient(
	configPath, proxyAddr string,
	timeout time.Duration,
	scanQR bool,
	fn func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error,
) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	if scanQR && cfg.BotToken != "" {
		return errors.New("--qr 仅支持用户账号，请配置 phone；机器人请使用普通 info")
	}
	deviceID, err := loadDeviceID(configPath)
	if err != nil {
		return fmt.Errorf("初始化设备标识失败: %w", err)
	}
	store, err := tools.OpenSessionStore(cfg.SessionPath())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// If an interactive stdin read has not returned yet, a subsequent signal
	// must still be able to terminate the command.
	defer context.AfterFunc(signalCtx, stop)()
	ctx := signalCtx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	unauthorized := errors.New("现有会话尚未授权")
	run := func(storage session.Storage, probe bool) error {
		var updates telegram.UpdateHandler
		var loggedIn qrlogin.LoggedIn
		if scanQR && !probe {
			dispatcher := tg.NewUpdateDispatcher()
			loggedIn = watchQRLogin(dispatcher, os.Stderr)
			updates = dispatcher
		}
		client, err := NewTelegramClient(cfg, proxyAddr, deviceID, storage, updates)
		if err != nil {
			return err
		}
		return client.Run(ctx, func(ctx context.Context) error {
			if scanQR {
				if !probe {
					if err := authorizeQR(ctx, client, cfg, loggedIn); err != nil {
						return err
					}
					fmt.Fprintln(os.Stderr, "Telegram 已授权，正在核验登录账号…")
				}
				user, err := client.Self(ctx)
				if probe && auth.IsUnauthorized(err) {
					return unauthorized
				}
				if err != nil {
					return fmt.Errorf("核验扫码账号失败: %w", err)
				}
				if err := saveQRSession(ctx, cfg.Phone, user, storage, store); err != nil {
					return err
				}
			} else if err := Authorize(ctx, client, cfg); err != nil {
				return err
			}
			return fn(ctx, client, cfg, store)
		})
	}
	if !scanQR {
		err = run(store, false)
	} else {
		payload, loadErr := store.LoadSession(ctx)
		if loadErr != nil && !errors.Is(loadErr, session.ErrNotFound) {
			return fmt.Errorf("读取现有会话失败: %w", loadErr)
		}
		if len(payload) > 0 {
			existing := new(session.StorageMemory)
			if err := existing.StoreSession(ctx, payload); err != nil {
				return err
			}
			err = run(existing, true)
		}
		if len(payload) == 0 || errors.Is(err, unauthorized) {
			// 使用全新的授权密钥；误扫的账号不能给磁盘上的旧密钥授权。
			err = run(new(session.StorageMemory), false)
		}
	}
	// gotd 可能把 context.Canceled 当成正常退出并返回 nil。
	if signalCtx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
		return errInterrupted
	}
	if scanQR && errors.Is(ctx.Err(), context.DeadlineExceeded) && (err == nil || errors.Is(err, context.DeadlineExceeded)) {
		return fmt.Errorf("扫码登录超时（%s），请确认手机已扫码并完成验证，或检查网络与代理: %w", timeout, ctx.Err())
	}
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("操作超时（%s），请检查网络与代理是否可用: %w", timeout, err)
	}
	return err
}
