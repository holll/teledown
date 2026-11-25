package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	contribauth "github.com/gotd/contrib/auth"
	"github.com/gotd/contrib/auth/localization"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/mattn/go-colorable"
	"golang.org/x/net/proxy"
	"golang.org/x/term"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"rsc.io/qr"
)

// 商品名取自 Google Play 公开设备清单。保留顺序，避免既有 ID 换型号。
var androidPhoneModels = [...]string{
	"Google Pixel 7", "Google Pixel 7 Pro", "Google Pixel 8", "Google Pixel 8 Pro",
	"Google Pixel 8a", "Google Pixel 9", "Google Pixel 9 Pro", "Google Pixel 9a",
	"Samsung Galaxy S23", "Samsung Galaxy S23 Ultra", "Samsung Galaxy S24", "Samsung Galaxy S24 Ultra",
	"Samsung Galaxy S25", "Samsung Galaxy S25 Ultra", "Xiaomi 14", "Xiaomi 15",
}

func androidDeviceModel(deviceID string) string {
	// 设备 ID 只用于本地选型，不拼进上报的型号名称。
	digest := sha256.Sum256([]byte(deviceID))
	return androidPhoneModels[int(digest[0])%len(androidPhoneModels)]
}

// NewTelegramClient 构建客户端：会话落盘、可选 SOCKS5 代理、开启 CDN 下载流程。
//
// proxyOverride 非空时优先于 .env 中的 proxy，格式与 Python 版一致：
//
//	socks5://user:pass@host:port
//	user:pass@host:port
//	host:port
func NewTelegramClient(cfg *Config, proxyOverride, deviceID string, store session.Storage, updates telegram.UpdateHandler, waitPolicy telegram.Middleware) (*telegram.Client, error) {
	opts := telegram.Options{
		SessionStorage: store,
		UpdateHandler:  updates,
		// 只有用户账号开启 CDN 重定向流程：服务端对 bot 账号限制 upload.getCdnFile，
		// 一旦被重定向，bot 既无法从 CDN 取数也无法回退，等于把「可能能下」变成「必定失败」。
		// 因此 bot_token 登录时显式关闭，继续走主 DC。
		AllowCDN: cfg.BotToken == "",
		// 自动消化 FLOOD_WAIT：gotd 默认不处理，会把 420 错误直接抛出。
		Middlewares: []telegram.Middleware{waitPolicy},
	}

	// 仅设置设备显示资料，继续使用配置中的应用凭据，不冒充 Telegram 官方客户端。
	opts.Device.DeviceModel = androidDeviceModel(deviceID)
	opts.Device.SystemVersion = "Android (simulated)"

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
		// url.Parse errors can contain the complete URL including its password.
		return nil, errors.New("无法解析 SOCKS5 代理地址，请检查格式及账号密码的 URL 编码")
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, errors.New("代理协议必须为 socks5 或 socks5h")
	}
	if u.Hostname() == "" {
		return nil, errors.New("代理地址缺少主机名")
	}
	if u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(addr, "#") {
		return nil, errors.New("SOCKS5 代理地址不支持路径、查询参数或片段")
	}
	if strings.Contains(u.Hostname(), ":") && !strings.HasPrefix(u.Host, "[") {
		return nil, errors.New("IPv6 代理主机名必须用方括号包围")
	}
	if rawPort := u.Port(); rawPort != "" {
		if port, err := strconv.ParseUint(rawPort, 10, 16); err != nil || port == 0 {
			return nil, errors.New("SOCKS5 代理端口必须在 1 到 65535 之间")
		}
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

	err = authFlow(cfg).Run(ctx, client.Auth())
	switch {
	case err == nil:
		return nil
	case tg.IsPasswordHashInvalid(err):
		return fmt.Errorf("两步验证密码不正确：请检查 .env 里 password 的值（%w）", err)
	case isSignUpRequired(err):
		return fmt.Errorf("该手机号在 Telegram 上没有可用账号（服务端要求先注册/接受条款）：本工具只支持已注册账号，请确认 .env 里的 phone 是否正确（%w）", err)
	case tg.IsAuthRestart(err):
		return fmt.Errorf("Telegram 要求重新开始登录（AUTH_RESTART）；本工具不会自动再次发码。可使用 info --qr 登录，无需删除已有会话库（%w）", err)
	default:
		return fmt.Errorf("登录失败: %w", err)
	}
}

type qrServerClock struct {
	clock.Clock
	offset func() time.Duration
}

func (c qrServerClock) Now() time.Time {
	// gotd 将剩余时间向下取整到秒，留一秒余量避免在到期前刷新。
	return c.Clock.Now().Add(c.offset() - time.Second)
}

func watchQRLogin(dispatcher interface{ OnLoginToken(tg.LoginTokenHandler) }, out io.Writer) qrlogin.LoggedIn {
	loggedIn := make(chan struct{}, 1)
	var once sync.Once
	dispatcher.OnLoginToken(func(context.Context, tg.Entities, *tg.UpdateLoginToken) error {
		once.Do(func() {
			fmt.Fprintln(out, "收到 Telegram 登录状态更新，正在完成授权…")
			loggedIn <- struct{}{}
		})
		return nil
	})
	return loggedIn
}

func distinctLoginQR(out io.Writer, show func(context.Context, qrlogin.Token) error) func(context.Context, qrlogin.Token) error {
	var lastToken string
	var lastExpiry time.Time
	var reported bool
	return func(ctx context.Context, token qrlogin.Token) error {
		value, expiry := token.String(), token.Expires()
		if value == lastToken && expiry.Equal(lastExpiry) {
			if !reported {
				fmt.Fprintln(out, "刷新后仍未取得授权结果，二维码未变。")
				reported = true
			}
			return nil
		}
		if err := show(ctx, token); err != nil {
			return err
		}
		lastToken, lastExpiry, reported = value, expiry, false
		return nil
	}
}

func authorizeQR(ctx context.Context, client *telegram.Client, cfg *Config, loggedIn qrlogin.LoggedIn) error {
	login := qrlogin.NewQR(client.API(), cfg.APIID, cfg.APIHash, qrlogin.Options{
		Clock: qrServerClock{Clock: clock.System, offset: client.ServerTimeOffset},
		Migrate: func(ctx context.Context, dc int) error {
			fmt.Fprintf(os.Stderr, "正在切换到数据中心 DC %d 完成授权…\n", dc)
			return client.MigrateTo(ctx, dc)
		},
		Password: func(ctx context.Context) (string, error) {
			fmt.Fprintln(os.Stderr, "Telegram 要求完成两步验证。")
			if cfg.Password != "" {
				return cfg.Password, ctx.Err()
			}
			return readQRPassword(ctx)
		},
	})
	_, err := login.Auth(ctx, loggedIn, distinctLoginQR(os.Stderr, func(_ context.Context, token qrlogin.Token) error {
		width, _, err := term.GetSize(int(os.Stderr.Fd()))
		if err != nil {
			return errors.New("扫码登录需要直接在终端中运行，不要重定向 stderr")
		}
		return showLoginQR(colorable.NewColorableStderr(), token, width)
	}))
	if tg.IsPasswordHashInvalid(err) {
		return fmt.Errorf("两步验证密码不正确：请检查 .env 里的 password（%w）", err)
	}
	if err != nil {
		return fmt.Errorf("扫码登录失败: %w", err)
	}
	return nil
}

func readQRPassword(ctx context.Context) (string, error) {
	return readTerminalLoginSecret(ctx, "该账号启用了两步验证，请输入密码（不回显）: ")
}

func readTerminalLoginSecret(ctx context.Context, prompt string) (password string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("此登录输入需要直接在终端中进行，不支持重定向 stdin")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("准备不回显的密码输入: %w", err)
	}
	defer func() {
		err = errors.Join(err, term.Restore(fd, state))
	}()

	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{qrPasswordReader{ctx: ctx, input: os.Stdin}, colorable.NewColorableStderr()}, "")
	password, err = terminal.ReadPassword(prompt)
	if errors.Is(err, io.EOF) {
		return "", errInterrupted
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return password, err
}

type qrPasswordReader struct {
	ctx   context.Context
	input io.Reader
}

func (r qrPasswordReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// 取消时系统读可能仍阻塞，使用独立缓冲区；终端状态由调用方恢复。
	type result struct {
		n   int
		err error
	}
	buf := make([]byte, len(p))
	done := make(chan result, 1)
	go func() {
		n, err := r.input.Read(buf)
		done <- result{n, err}
	}()
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case res := <-done:
		copy(p, buf[:res.n])
		return res.n, res.err
	}
}

func saveQRSession(ctx context.Context, expectedPhone string, user *tg.User, pending, store session.Storage) error {
	phone := strings.TrimPrefix(expectedPhone, "+")
	if user.Bot || phone == "" || strings.TrimPrefix(user.Phone, "+") != phone {
		return errors.New("登录账号与 .env 中的 phone 不一致或无法核实，未保存会话；请确认手机上的当前账号")
	}
	payload, err := pending.LoadSession(ctx)
	if err != nil {
		return fmt.Errorf("读取扫码会话失败: %w", err)
	}
	if err := store.StoreSession(ctx, payload); err != nil {
		return fmt.Errorf("保存扫码会话失败: %w", err)
	}
	return nil
}

func loginQRURL(token qrlogin.Token) string {
	// Telethon 生成的扫码链接使用不带尾部填充的 Base64URL。
	return strings.TrimRight(token.URL(), "=")
}

func showLoginQR(w io.Writer, token qrlogin.Token, width int) error {
	code, err := qr.Encode(loginQRURL(token), qr.M)
	if err != nil {
		return fmt.Errorf("生成登录二维码失败: %w", err)
	}
	const margin = 4
	size := code.Size + 2*margin
	if width <= size {
		return fmt.Errorf("终端太窄，扫码登录需要至少 %d 列；请拉宽窗口后重试", size+1)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\n在已登录的手机 Telegram 中打开「设置 → 设备 → 连接桌面设备」，扫描最新二维码（%s 到期）。不要分享二维码。\n",
		token.Expires().Local().Format("15:04:05"))
	glyphs := [...]rune{' ', '▀', '▄', '█'}
	for y := -margin; y < code.Size+margin; y += 2 {
		out.WriteString("\x1b[30;107m")
		for x := -margin; x < code.Size+margin; x++ {
			var pixel int
			if code.Black(x, y) {
				pixel |= 1
			}
			if code.Black(x, y+1) {
				pixel |= 2
			}
			out.WriteRune(glyphs[pixel])
		}
		out.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(w, out.String())
	return err
}

// configCredentials 提供认证流程里的手机号与两步验证密码。
// 验证码与密码输入由 CLI 负责；条款和注册由 contrib/auth 拒绝。
type configCredentials struct {
	cfg          *Config
	readPassword func(context.Context) (string, error)
}

var _ contribauth.Credentials = configCredentials{}

func (c configCredentials) Phone(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.cfg.Phone == "" {
		return "", errors.New("请在 .env 中填写 phone")
	}
	return c.cfg.Phone, nil
}

// Password 优先用 .env 里配置的密码；未配置时才交互式输入（不回显）。
//
// 不能用 auth.Constant：它会把配置里的密码（哪怕是空串）直接提交给服务端，
// 启用两步验证的账号会得到一个含糊的「invalid password」而不是提示补密码。
func (c configCredentials) Password(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.cfg.Password != "" {
		return c.cfg.Password, nil
	}
	if c.readPassword == nil {
		return "", errors.New("未配置两步验证密码输入")
	}
	value, err := c.readPassword(ctx)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return value, err
}

// authFlow 组装登录流程：
//   - 手机号与两步验证密码取 .env（configCredentials）；
//   - 验证码和两步密码使用可取消的输入，终端不回显；
//   - 条款确认与注册流程用 contrib/auth 的 NoSignUp 明确拒绝。
func authFlow(cfg *Config) auth.Flow {
	read := readTerminalLoginSecret
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		read = pipedLoginReader(os.Stdin, os.Stderr)
	}
	printer := chinesePrinter()
	code := loginCodePrompt(os.Stderr, func(ctx context.Context, sent *tg.AuthSentCode) (string, error) {
		return readLoginCode(ctx, sent, read, os.Stderr, printer)
	})
	ask := contribauth.BuildAsk(code, contribauth.NoSignUp())
	credentials := configCredentials{cfg: cfg, readPassword: func(ctx context.Context) (string, error) {
		return read(ctx, "该账号启用了两步验证，请输入密码: ")
	}}
	return auth.NewFlow(contribauth.Build(credentials, ask), auth.SendCodeOptions{})
}

func readLoginCode(ctx context.Context, sent *tg.AuthSentCode, read func(context.Context, string) (string, error), out io.Writer, printer *message.Printer) (string, error) {
	for {
		value, err := read(ctx, printer.Sprintf(localization.CodeDialogPrompt)+": ")
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if sized, ok := sent.Type.(interface{ GetLength() int }); ok && sized.GetLength() > 0 && len(value) != sized.GetLength() {
			if _, err := fmt.Fprintln(out, printer.Sprintf(localization.CodeInvalidLength, sized.GetLength())); err != nil {
				return "", err
			}
			continue
		}
		if value != "" {
			return value, nil
		}
		if _, err := fmt.Fprintln(out, "验证码不能为空"); err != nil {
			return "", err
		}
	}
}

func pipedLoginReader(in io.Reader, out io.Writer) func(context.Context, string) (string, error) {
	// A flow never calls read again after cancellation. Keep one buffer for
	// both steps so a prefetched password is not lost.
	input := bufio.NewReader(in)
	return func(ctx context.Context, prompt string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, err := io.WriteString(out, prompt); err != nil {
			return "", err
		}
		type result struct {
			line string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			line, err := readSecretLine(input)
			done <- result{line, err}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case res := <-done:
			return res.line, res.err
		}
	}
}

func loginCodePrompt(w io.Writer, read auth.CodeAuthenticatorFunc) auth.CodeAuthenticatorFunc {
	return func(ctx context.Context, sent *tg.AuthSentCode) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		printLoginCodeDelivery(w, sent)
		if sent == nil || sent.Type == nil {
			return "", errors.New("Telegram 未返回有效的验证码渠道；可使用 info --qr 登录")
		}
		switch sent.Type.(type) {
		case *tg.AuthSentCodeTypeApp, *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord,
			*tg.AuthSentCodeTypeSMSPhrase, *tg.AuthSentCodeTypeCall:
			// These channels accept an ordinary phone_code on auth.signIn.
		default:
			return "", errors.New("当前登录渠道不能通过本工具的普通验证码流程完成；请使用 info --qr 登录，或在官方客户端完成所需验证")
		}
		value, err := read(ctx, sent)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return value, err
	}
}

func printLoginCodeDelivery(w io.Writer, sent *tg.AuthSentCode) {
	if sent == nil || sent.Type == nil {
		return
	}
	// 只输出固定类型名和非敏感元数据，不打印 RPC 对象或投递地址。
	channel := sent.Type.TypeName()
	switch sent.Type.(type) {
	case *tg.AuthSentCodeTypeApp:
		channel += "，请查看同一账号已登录客户端中的官方「Telegram」对话（不是短信）"
	case *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase:
		channel += "，请查看手机短信"
	case *tg.AuthSentCodeTypeEmailCode:
		channel += "，请查看登录邮箱；邮箱验证码需要单独的邮箱验证流程"
	case *tg.AuthSentCodeTypeCall:
		channel += "，请留意电话播报的验证码"
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		channel += "，服务端要求以拨入电话或未接来电验证"
	case *tg.AuthSentCodeTypeSetUpEmailRequired:
		channel += "，服务端要求先设置登录邮箱，尚未发送登录码"
	case *tg.AuthSentCodeTypeFirebaseSMS:
		channel += "，服务端要求移动端先完成验证，不能视为短信已发出"
	case *tg.AuthSentCodeTypeFragmentSMS:
		channel += "，服务端要求通过 Fragment 获取验证码"
	}
	fmt.Fprintf(w, "Telegram 返回的验证码投递类型：%s\n", channel)
	if code, ok := sent.Type.(interface{ GetLength() int }); ok && code.GetLength() > 0 {
		fmt.Fprintf(w, "验证码长度：%d\n", code.GetLength())
	}
	if next, ok := sent.GetNextType(); ok {
		fmt.Fprintf(w, "服务端提供的重发渠道：%s（本工具不会自动重发）\n", next.TypeName())
	} else {
		fmt.Fprintln(w, "服务端未提供重发渠道。")
	}
	if timeout, ok := sent.GetTimeout(); ok {
		fmt.Fprintf(w, "服务端等待时间：%d 秒（不是终端输入倒计时）\n", timeout)
	}
}

// isSignUpRequired 判断错误链里是否有「服务端要求注册」。
// NoSignUp 的 AcceptTermsOfService 返回 *auth.SignUpRequired，会被 flow 包上 "confirm TOS"，
// 所以这里用 errors.As 找链上任意一层。
func isSignUpRequired(err error) bool {
	var required *auth.SignUpRequired
	return errors.As(err, &required)
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

func readSecretLine(input io.Reader) (string, error) {
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}
