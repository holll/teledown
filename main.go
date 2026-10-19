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

	"github.com/gotd/td/telegram"

	"teledown-go/tools"
)

// infoTimeout 限制 info 命令的连接与登录耗时：代理不可达时 gotd 会持续重连，
// 不加时限会让「快速自检」变成无限等待。
const infoTimeout = 60 * time.Second

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

	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `teledown-go - 基于 gotd/td 的 Telegram 下载上传工具

用法:
  teledown-go info                                 查看当前账号登录状态
  teledown-go download -id <频道ID|@用户名> [--range <范围>] [--prefix <通配符>] [--since <日期>] [--until <日期>]
  teledown-go upload   -id <频道ID|@用户名|me> -path <文件或目录> [-dau Y] [-at <标签>]

通用参数（放在子命令之后）:
  -c <路径>     配置文件，默认 .env
  --proxy <地址>  SOCKS5 代理，格式 socks5://user:pass@host:port 或 host:port

download / upload 额外参数:
  -threads <n>  分片并发数，默认 4

--range 支持的写法:
  >100        消息 ID 大于 100
  <100        消息 ID 小于 100
  100         单条消息
  1,2,3       多条消息
  10..200     区间（等价 s10s200）

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

// runInfoCommand 只做登录与账号信息展示，供确认登录是否成功。
func runInfoCommand(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	config, proxyAddr := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// info 是「快速自检」，代理不可达时 gotd 会一直重连，必须加超时兜底
	return withClient(*config, *proxyAddr, infoTimeout, func(ctx context.Context, client *telegram.Client, cfg *Config, _ *tools.SessionStore) error {
		return showInfo(ctx, client, cfg)
	})
}

// showInfo 打印账号信息，对应 Python 版 main.py 的 show_info。
func showInfo(ctx context.Context, client *telegram.Client, cfg *Config) error {
	me, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("获取账号信息失败: %w", err)
	}

	loginMethod := "手机号 " + cfg.Phone
	if cfg.BotToken != "" {
		loginMethod = "机器人 Token"
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
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	config, proxyAddr := commonFlags(fs)
	threads := fs.Int("threads", tools.DefaultThreads, "分片并发数")
	channelID := fs.String("id", "", "频道 ID 或 @用户名")
	rangeArg := fs.String("range", ">0", "消息范围")
	prefix := fs.String("prefix", "", "文件名通配符，; 分隔多条")
	since := fs.String("since", "", "只下载该日期（含）之后的消息，如 2026-09-01")
	until := fs.String("until", "", "只下载该日期（含）之前的消息")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channelID == "" {
		return fmt.Errorf("必须指定 -id")
	}

	msgRange, err := tools.ParseRange(*rangeArg)
	if err != nil {
		return err
	}
	dateRange, err := tools.ParseDateRange(*since, *until)
	if err != nil {
		return err
	}

	return withClient(*config, *proxyAddr, 0, func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error {
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
			Threads:  *threads,
			Manager:  manager,
		})
	})
}

func runUploadCommand(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	config, proxyAddr := commonFlags(fs)
	threads := fs.Int("threads", tools.DefaultThreads, "分片并发数")
	channelID := fs.String("id", "", "频道 ID、@用户名或 me")
	path := fs.String("path", "", "要上传的文件或目录")
	deleteAfter := fs.String("dau", "N", "上传后是否删除源文件 (Y/N)")
	tag := fs.String("at", "", "追加到描述前的标签，不带 #")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("必须指定 -path")
	}
	if *channelID == "" {
		*channelID = "me"
	}
	deleteFlag := *deleteAfter == "Y" || *deleteAfter == "y"

	return withClient(*config, *proxyAddr, 0, func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error {
		peer, err := tools.NewPeerManager(client.API(), store).Resolve(ctx, *channelID)
		if err != nil {
			return err
		}
		return tools.Upload(ctx, client.API(), peer, tools.UploadOptions{
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
	fn func(ctx context.Context, client *telegram.Client, cfg *Config, store *tools.SessionStore) error,
) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	store, err := tools.OpenSessionStore(cfg.SessionPath())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// 首次运行（库里还没有会话）时把改造前的两个 JSON 导入，之后就不再读它们了。
	imported, err := store.ImportLegacy(cfg.LegacySessionPath(), cfg.LegacyPeerStoragePath())
	if err != nil {
		return err
	}
	if imported {
		fmt.Fprintf(os.Stderr, "已把旧的会话与 peer 缓存合并进 %s，原 .gotd.json / .peers.json 不再使用（可自行删除）\n",
			cfg.SessionPath())
	}

	client, err := NewTelegramClient(cfg, proxyAddr, store)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err = client.Run(ctx, func(ctx context.Context) error {
		if err := Authorize(ctx, client, cfg); err != nil {
			return err
		}
		return fn(ctx, client, cfg, store)
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("操作超时（%s），请检查网络与代理是否可用", timeout)
	}
	return err
}
