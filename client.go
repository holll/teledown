package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	contribauth "github.com/gotd/contrib/auth"
	"github.com/gotd/contrib/auth/localization"
	"github.com/gotd/contrib/auth/terminal"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"
	"golang.org/x/term"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"teledown-go/tools"
)

// 登录流程的重试参数：与 Telethon 的 send_code_request 重试策略一致（最多 3 次）。
const (
	authMaxAttempts  = 3
	authRestartDelay = 2 * time.Second
)

// NewTelegramClient 构建客户端：会话落盘、可选 SOCKS5 代理、开启 CDN 下载流程。
//
// store 是 Telethon 风格的 SQLite 会话库（auth key + DC + 实体缓存都在里面），由调用方
// 打开并负责关闭：下载/上传解析 peer 时还要用它当 peers.Storage。
//
// proxyOverride 非空时优先于 .env 中的 proxy，格式与 Python 版一致：
//
//	socks5://user:pass@host:port
//	user:pass@host:port
//	host:port
func NewTelegramClient(cfg *Config, proxyOverride string, store *tools.SessionStore) (*telegram.Client, error) {
	opts := telegram.Options{
		SessionStorage: store,
		// 只有用户账号开启 CDN 重定向流程：服务端对 bot 账号限制 upload.getCdnFile，
		// 一旦被重定向，bot 既无法从 CDN 取数也无法回退，等于把「可能能下」变成「必定失败」。
		// 因此 bot_token 登录时显式关闭，继续走主 DC。
		AllowCDN: cfg.BotToken == "",
		// 自动消化 FLOOD_WAIT：gotd 默认不处理，会把 420 错误直接抛出。
		Middlewares: []telegram.Middleware{floodWaitMiddleware{}},
	}

	proxyURL := proxyOverride
	if proxyURL == "" {
		proxyURL = cfg.Proxy
	}
	if proxyURL != "" {
		dial, err := socks5Dialer(proxyURL)
		if err != nil {
			return nil, err
		}
		opts.Resolver = dcs.Plain(dcs.PlainOptions{Dial: dial})
	}

	return telegram.NewClient(cfg.APIID, cfg.APIHash, opts), nil
}

// socks5Dialer 把代理地址转成 gotd 需要的拨号函数。
//
// 解析交给 x/net/proxy.FromURL：它认得 socks5 / socks5h、user:pass@ 认证，
// 并在没写端口时补默认的 1080（与 Python 版 python-socks 的默认值一致）。
// 这里只多做一件事——.env 里允许只写 host:port 不带 scheme，补一个再交给它。
func socks5Dialer(raw string) (dcs.DialFunc, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return nil, errors.New("代理地址为空")
	}
	if !strings.Contains(addr, "://") {
		addr = "socks5://" + addr
	}

	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("代理地址 %q 无法解析: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址 %q 缺少主机名", raw)
	}

	d, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建 SOCKS5 拨号器失败: %w", err)
	}
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 拨号器不支持上下文")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return ctxDialer.DialContext(ctx, network, address)
	}, nil
}

// Authorize 完成登录。已有会话时不会重复登录。
func Authorize(ctx context.Context, client *telegram.Client, cfg *Config) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("查询登录状态失败: %w", err)
	}
	if status.Authorized {
		return nil
	}

	if cfg.BotToken != "" {
		if _, err := client.Auth().Bot(ctx, cfg.BotToken); err != nil {
			return fmt.Errorf("机器人登录失败: %w", err)
		}
		return nil
	}

	flow := authFlow(cfg)

	// AUTH_RESTART 表示服务端要求从「发送验证码」这一步重来（上一次流程中断、
	// 或会话里留着未完成的登录状态时常见）。Telethon 的做法也是重试整个流程、最多 3 次。
	var lastErr error
	for attempt := 1; attempt <= authMaxAttempts; attempt++ {
		err := client.Auth().IfNecessary(ctx, flow)
		if err == nil {
			return nil
		}
		lastErr = err

		switch {
		case tg.IsPasswordHashInvalid(err):
			return fmt.Errorf("两步验证密码不正确：请检查 .env 里 password 的值（%w）", err)
		case isSignUpRequired(err):
			// 服务端要求注册新账号（手机号不对、或该号还没注册过）。
			// 本工具只登录已有账号，不代用户走注册流程。
			return fmt.Errorf("该手机号在 Telegram 上没有可用账号（服务端要求先注册/接受条款）：本工具只支持已注册账号，请确认 .env 里的 phone 是否正确（%w）", err)
		case tg.IsAuthRestart(err) && attempt < authMaxAttempts:
			fmt.Printf("服务端要求重新开始登录流程（AUTH_RESTART），%s 后重试（第 %d/%d 次）\n",
				authRestartDelay, attempt+1, authMaxAttempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(authRestartDelay):
			}
		case tg.IsAuthRestart(err):
			// 重试仍失败：多半是会话文件里残留了未完成的登录状态
			return fmt.Errorf("登录流程反复被要求重启（AUTH_RESTART）：请删除会话文件 %s 后重试（%w）",
				cfg.SessionPath(), err)
		default:
			return fmt.Errorf("登录失败: %w", err)
		}
	}
	return fmt.Errorf("登录失败: %w", lastErr)
}

// configCredentials 提供认证流程里的手机号与两步验证密码。
// 交互部分（验证码读取、位数校验、条款与注册流程）由 contrib/auth/terminal 负责。
type configCredentials struct {
	cfg *Config
}

var _ contribauth.Credentials = configCredentials{}

func (c configCredentials) Phone(context.Context) (string, error) {
	if c.cfg.Phone == "" {
		return "", errors.New("请在 .env 中填写 phone")
	}
	return c.cfg.Phone, nil
}

// Password 优先用 .env 里配置的密码；未配置时才交互式输入（不回显）。
//
// 不能用 auth.Constant：它会把配置里的密码（哪怕是空串）直接提交给服务端，
// 启用两步验证的账号会得到一个含糊的「invalid password」而不是提示补密码。
func (c configCredentials) Password(context.Context) (string, error) {
	if c.cfg.Password != "" {
		return c.cfg.Password, nil
	}
	return promptSecret("该账号启用了两步验证，请输入密码（不回显）: ")
}

// authFlow 组装登录流程：
//   - 手机号与两步验证密码取 .env（configCredentials）；
//   - 验证码的提示、读取与位数校验用 contrib/auth/terminal（tty 检测、管道回退都是现成的）；
//   - 条款确认与注册流程用 contrib/auth 的 NoSignUp 明确拒绝。
//
// 不能直接把 terminal.OS() 当 Ask 传：它自带条款 Y/N 与注册（问名/姓）流程，
// 手机号写错时会把号码往注册流程上带。本工具只用于已登录账号的下载/上传。
func authFlow(cfg *Config) auth.Flow {
	ask := contribauth.BuildAsk(newTerminalAuth(), contribauth.NoSignUp())
	return auth.NewFlow(contribauth.Build(configCredentials{cfg: cfg}, ask), auth.SendCodeOptions{})
}

// isSignUpRequired 判断错误链里是否有「服务端要求注册」。
// NoSignUp 的 AcceptTermsOfService 返回 *auth.SignUpRequired，会被 flow 包上 "confirm TOS"，
// 所以这里用 errors.As 找链上任意一层。
func isSignUpRequired(err error) bool {
	var required *auth.SignUpRequired
	return errors.As(err, &required)
}

// newTerminalAuth 构造终端交互式认证，并把提示语换成中文（contrib 自带 catalog 只有英文）。
func newTerminalAuth() *terminal.Terminal {
	return terminal.OS().WithPrinter(chinesePrinter())
}

// chinesePrinter 在 contrib 自带的 catalog（只有英文）上补中文词条。
// 未覆盖到的键会回退成 catalog 里的英文原文，不会显示成键名。
func chinesePrinter() *message.Printer {
	b := localization.Catalog()
	zh := language.Chinese
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(b.SetString(zh, localization.PhoneDialogPrompt, "手机号"))
	must(b.SetString(zh, localization.PasswordDialogPrompt, "密码"))
	must(b.SetString(zh, localization.CodeDialogPrompt, "验证码"))
	must(b.SetString(zh, localization.CodeInvalidLength, "验证码位数不对，应为 %d 位"))
	must(b.SetString(zh, localization.TOSDialogTitle, "Telegram 要求先完成注册/接受条款"))
	must(b.SetString(zh, localization.TOSDialogPrompt, "接受"))
	must(b.SetString(zh, localization.FirstNameDialogPrompt, "名"))
	must(b.SetString(zh, localization.SecondNameDialogPrompt, "姓"))
	return message.NewPrinter(zh, message.Catalog(b))
}

// promptSecret 读取不回显的敏感输入；非终端环境（如管道）退回普通读取。
//
// 两步验证密码不走 contrib 的 Terminal：那边用普通 ReadLine，会把密码显示在屏幕上。
func promptSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
