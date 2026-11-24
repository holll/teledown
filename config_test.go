package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/contrib/auth/terminal"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
)

// 只覆盖不触发终端输入的分支：配置了值就必须直接用配置，不能再去提示用户。
func TestConfigCredentialsUsesConfiguredValues(t *testing.T) {
	cred := configCredentials{cfg: &Config{Phone: "+8613800138000", Password: "s3cret"}}

	phone, err := cred.Phone(context.Background())
	if err != nil || phone != "+8613800138000" {
		t.Errorf("Phone() = %q, %v；应直接返回 .env 中的手机号", phone, err)
	}

	password, err := cred.Password(context.Background())
	if err != nil || password != "s3cret" {
		t.Errorf("Password() = %q, %v；已配置密码时不应再走交互输入", password, err)
	}
}

// 未配置手机号要立刻报错，而不是把空串交给服务端（那样只会得到一个含糊的错误码）。
func TestConfigCredentialsRequiresPhone(t *testing.T) {
	if _, err := (configCredentials{cfg: &Config{}}).Phone(context.Background()); err == nil {
		t.Error("未配置 phone 时应报错")
	}
}

// 验证码读取与位数校验由 contrib/auth/terminal 提供（不再自己实现）：
// 位数不对时重新提示，且提示语用的是我们补的中文词条。
func TestTerminalAuthCodeValidation(t *testing.T) {
	var out strings.Builder
	// 第一次输入 2 位（服务端说 4 位），第二次输入正确的
	term := terminal.New(strings.NewReader("12\n1234\n"), &out).WithPrinter(chinesePrinter())

	code, err := term.Code(context.Background(), &tg.AuthSentCode{
		Type: &tg.AuthSentCodeTypeApp{Length: 4},
	})
	if err != nil {
		t.Fatalf("Code() 报错: %v", err)
	}
	if code != "1234" {
		t.Errorf("Code() = %q，期望取到位数正确的那次输入", code)
	}
	if got := out.String(); !strings.Contains(got, "验证码") || !strings.Contains(got, "位数") {
		t.Errorf("中文提示未生效，实际输出 %q", got)
	}
}

func TestQRPasswordReadStopsAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		input, writer := io.Pipe()
		defer input.Close()
		defer writer.Close()
		var buf [16]byte
		n, err := (qrPasswordReader{ctx: ctx, input: input}).Read(buf[:])
		if n != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("密码读取应在截止时间返回超时，得到 %d, %v", n, err)
		}
		if _, err := writer.Write([]byte("late input")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if buf != ([16]byte{}) {
			t.Fatal("取消后不应写入调用方的缓冲区")
		}
	})
}

func TestLoginQRURLMatchesTelethonEncoding(t *testing.T) {
	for _, tc := range []struct {
		data []byte
		want string
	}{
		{[]byte{0xfb}, "-w"},
		{[]byte{0xfb, 0xff}, "-_8"},
		{[]byte{0xfb, 0xff, 0xff}, "-___"},
	} {
		token := qrlogin.NewToken(tc.data, 100)
		if got := loginQRURL(token); got != "tg://login?token="+tc.want {
			t.Errorf("合成令牌的链接编码 = %q，期望后缀 %q", got, tc.want)
		}
	}
}

func TestShowLoginQRDoesNotPrintRawToken(t *testing.T) {
	var out strings.Builder
	token := qrlogin.NewToken([]byte("synthetic-login-token"), int(time.Now().Add(time.Minute).Unix()))
	if err := showLoginQR(&out, token, 20); err == nil || out.Len() != 0 {
		t.Fatal("终端太窄时不应输出二维码")
	}
	if err := showLoginQR(&out, token, 80); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "连接桌面设备") || !strings.Contains(got, "\x1b[30;107m    ") || !strings.Contains(got, "█") {
		t.Fatal("扫码提示或二维码缺失")
	}
	if strings.Contains(got, "tg://") || strings.Contains(got, token.String()) {
		t.Fatal("不应输出二维码原始令牌")
	}
}

func TestDistinctLoginQRReportsUnchangedTokenOnce(t *testing.T) {
	var out strings.Builder
	var rendered int
	show := distinctLoginQR(&out, func(context.Context, qrlogin.Token) error {
		rendered++
		return nil
	})
	first := qrlogin.NewToken([]byte("first-test-token"), 100)
	second := qrlogin.NewToken([]byte("second-test-token"), 100)
	extended := qrlogin.NewToken([]byte("second-test-token"), 130)
	for _, token := range []qrlogin.Token{first, first, first, second, second, extended} {
		if err := show(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	if rendered != 3 || strings.Count(out.String(), "二维码未变") != 2 {
		t.Fatalf("绘制次数 = %d，诊断 = %q", rendered, out.String())
	}
	if strings.Contains(out.String(), first.String()) || strings.Contains(out.String(), second.String()) {
		t.Fatal("诊断不应输出登录令牌")
	}
}

func TestWatchQRLoginRetainsEarlyUpdate(t *testing.T) {
	var out strings.Builder
	dispatcher := new(qrLoginHandlerCapture)
	loggedIn := watchQRLogin(dispatcher, &out)
	for range 2 {
		if err := dispatcher.handler(t.Context(), tg.Entities{}, &tg.UpdateLoginToken{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-loggedIn:
	default:
		t.Fatal("等待开始前收到的更新也应保留")
	}
	if strings.Count(out.String(), "收到 Telegram 登录状态更新") != 1 {
		t.Fatalf("状态提示应只输出一次：%q", out.String())
	}
}

type qrLoginHandlerCapture struct{ handler tg.LoginTokenHandler }

func (c *qrLoginHandlerCapture) OnLoginToken(h tg.LoginTokenHandler) { c.handler = h }

func TestSaveQRSessionRequiresMatchingUser(t *testing.T) {
	ctx := t.Context()
	pending, store := new(session.StorageMemory), new(session.StorageMemory)
	if err := pending.StoreSession(ctx, []byte("new session")); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreSession(ctx, []byte("old session")); err != nil {
		t.Fatal(err)
	}
	const phone = "15550100000"
	for _, user := range []tg.User{{Phone: "15550100001"}, {}, {Phone: phone, Bot: true}} {
		if err := saveQRSession(ctx, "+"+phone, &user, pending, store); err == nil {
			t.Fatal("拒绝错误账号或未提供手机号的账号")
		}
		if saved, err := store.LoadSession(ctx); err != nil || string(saved) != "old session" {
			t.Fatal("拒绝时不应覆盖已有会话")
		}
	}
	if err := saveQRSession(ctx, "+"+phone, &tg.User{Phone: phone}, pending, store); err != nil {
		t.Fatal(err)
	}
	if saved, err := store.LoadSession(ctx); err != nil || string(saved) != "new session" {
		t.Fatal("匹配账号应写入已授权会话")
	}
}

func TestInfoQRRejectsBotBeforeOpeningSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("api_id=123\napi_hash=test\nbot_token=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInfoCommand([]string{"--qr", "-c", path}); err == nil || !strings.Contains(err.Error(), "仅支持用户账号") {
		t.Fatalf("机器人扫码模式错误 = %v", err)
	}
}

func TestLoginCodeDeliveryShowsChannelAndResendMetadata(t *testing.T) {
	var out strings.Builder
	sent := &tg.AuthSentCode{Type: &tg.AuthSentCodeTypeApp{Length: 5}, PhoneCodeHash: "private-code-hash"}
	sent.SetNextType(&tg.AuthCodeTypeSMS{})
	sent.SetTimeout(60)
	printLoginCodeDelivery(&out, sent)
	got := out.String()
	for _, want := range []string{"auth.sentCodeTypeApp", "官方「Telegram」对话（不是短信）", "验证码长度：5",
		"重发渠道：auth.codeTypeSms", "不会自动重发", "服务端等待时间：60 秒"} {
		if !strings.Contains(got, want) {
			t.Errorf("诊断缺少 %q：%q", want, got)
		}
	}
	if strings.Contains(got, sent.PhoneCodeHash) {
		t.Fatal("不应输出验证码哈希")
	}
}

func TestLoginCodeDeliveryDoesNotExposeEmailOrMissingTimeout(t *testing.T) {
	var out strings.Builder
	const emailPattern = "secret***@example.invalid"
	printLoginCodeDelivery(&out, &tg.AuthSentCode{
		Type: &tg.AuthSentCodeTypeEmailCode{EmailPattern: emailPattern, Length: 6},
	})
	got := out.String()
	if !strings.Contains(got, "请查看登录邮箱") || !strings.Contains(got, "未提供重发渠道") ||
		strings.Contains(got, emailPattern) || strings.Contains(got, "服务端等待时间") {
		t.Errorf("邮箱诊断不符合预期：%q", got)
	}
}

// 登录流程只认已注册账号：条款确认与注册都必须被拒绝。
//
// 这里刻意断言错误是 *auth.SignUpRequired —— 它是 contrib 的 NoSignUp 的返回值。
// 如果哪天误把 terminal.OS() 整个当 Ask 传进来，AcceptTermsOfService 会改成尝试读取
// 用户输入，错误类型随之变化，这个断言就会失败。
func TestAuthFlowRefusesSignUpAndTerms(t *testing.T) {
	flow := authFlow(&Config{Phone: "+8613800138000", Password: "s3cret"})

	phone, err := flow.Auth.Phone(context.Background())
	if err != nil || phone != "+8613800138000" {
		t.Errorf("Phone() = %q, %v；应取 .env 里的手机号", phone, err)
	}

	tosErr := flow.Auth.AcceptTermsOfService(context.Background(), tg.HelpTermsOfService{})
	if tosErr == nil {
		t.Fatal("条款确认必须被拒绝，不能静默接受")
	}
	if !isSignUpRequired(tosErr) {
		t.Errorf("条款被拒时应返回 *auth.SignUpRequired（由 contrib 的 NoSignUp 提供），得到 %v", tosErr)
	}

	if _, err := flow.Auth.SignUp(context.Background()); err == nil {
		t.Error("注册流程必须被拒绝")
	}
}

func TestSocks5DialerForms(t *testing.T) {
	valid := []string{
		"127.0.0.1:7890",
		"socks5://127.0.0.1:7890",
		"user:pass@127.0.0.1:7890",
		"socks5://user:pass@127.0.0.1:7890",
		// 下面两种靠 x/net/proxy.FromURL：不写 scheme 时补 socks5://，
		// 不写端口时补 1080（与 Python 版 python-socks 的默认值一致）
		"socks5h://127.0.0.1:7890",
		"socks5://127.0.0.1",
		"socks5://user:pass@127.0.0.1",
	}
	for _, in := range valid {
		if _, err := socks5Dialer(in); err != nil {
			t.Errorf("socks5Dialer(%q) 不应报错: %v", in, err)
		}
	}

	for _, in := range []string{"", "socks5://", "socks5://user:pass@"} {
		if _, err := socks5Dialer(in); err == nil {
			t.Errorf("socks5Dialer(%q) 应报错", in)
		}
	}
}

func TestParseEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# 注释\nAPI_ID=123456\nAPI_HASH=\"abc\"\nphone='13800000000'\n\nSAVE_PATH= D:/downloads \n没有等号的行\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"api_id":    "123456",
		"api_hash":  "abc",
		"phone":     "13800000000",
		"save_path": "D:/downloads",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("配置项 %s = %q，期望 %q", k, env[k], v)
		}
	}
	if _, ok := env["没有等号的行"]; ok {
		t.Error("没有等号的行不应产生配置项")
	}
}

func TestAndroidDeviceModelOnlyReturnsCatalogName(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	name := androidDeviceModel(id)
	if name != androidDeviceModel(id) {
		t.Fatal("应为同一设备 ID 返回稳定的型号")
	}
	for _, model := range androidPhoneModels {
		if name == model {
			return
		}
	}
	t.Fatalf("设备名称应只包含清单中的型号，得到 %q", name)
}

func TestDeviceIDConcurrentInitializationAndReuse(t *testing.T) {
	dir := t.TempDir()
	const count = 8
	type result struct {
		id  string
		err error
	}
	results := make(chan result, count)
	for range count {
		go func() {
			id, err := loadDeviceID(filepath.Join(dir, ".env"))
			results <- result{id, err}
		}()
	}
	var first string
	for range count {
		r := <-results
		id, err := r.id, r.err
		if err != nil || len(id) != 32 {
			t.Fatalf("生成设备标识失败：长度 %d，错误 %v", len(id), err)
		}
		if first == "" {
			first = id
		} else if id != first {
			t.Fatal("并发启动必须共用一个设备标识")
		}
	}
	if id, err := loadDeviceID(filepath.Join(dir, "other.env")); err != nil || id != first {
		t.Fatalf("同一配置目录应复用标识，错误 %v", err)
	}
	if id, err := loadDeviceID(filepath.Join(t.TempDir(), ".env")); err != nil || id == first {
		t.Fatalf("另一配置目录应另建标识，错误 %v", err)
	}
}

func TestDeviceIDDoesNotReplaceInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.id")
	const invalid = "invalid"
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeviceID(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("应拒绝格式错误的设备标识")
	}
	if saved, err := os.ReadFile(path); err != nil || string(saved) != invalid {
		t.Fatalf("不应修改已有设备标识文件，错误 %v", err)
	}
}

func TestSessionPathUsesStandaloneFilename(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		id   string
	}{
		{"phone", Config{Phone: "13800138000"}, "7945bd83237335e5"},
		{"bot", Config{BotToken: "123:abc"}, "ebecf09cd7c66130"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.SessionName(); got != tc.id {
				t.Errorf("SessionName() = %q, want %q", got, tc.id)
			}
			if got, want := tc.cfg.SessionPath(), tc.id+".teledown.sqlite3"; got != want {
				t.Errorf("SessionPath() = %q, want %q", got, want)
			}
		})
	}
}

func TestLoadConfigValidatesAPIID(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	for _, id := range []string{"-1", "0", "2147483648", "1.5"} {
		if err := os.WriteFile(path, []byte("API_ID="+id+"\nAPI_HASH=hash\nPHONE=123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("accepted API_ID %q", id)
		}
	}
	if err := os.WriteFile(path, []byte("\ufeffexport\tAPI_ID=2147483647 # comment\r\nAPI_HASH=hash#literal\r\nPHONE=123\r\nPASSWORD=' p#ass ' # comment\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIID != 2147483647 || cfg.APIHash != "hash#literal" || cfg.Password != " p#ass " {
		t.Fatal("BOM, comments, or quoted whitespace parsed incorrectly")
	}
}

func TestSocks5DialerValidationDoesNotExposePassword(t *testing.T) {
	for _, addr := range []string{
		"socks5://u:password%invalid@host:1080", "http://u:password@host:1080",
		"socks5://u:password@host:0", "socks5://u:password@host:99999",
		"socks5://u:password@host:8080/path", "socks5://u:password@host#",
		"socks5://2001:db8::1:1080",
	} {
		if _, err := socks5Dialer(addr); err == nil || strings.Contains(err.Error(), "password") {
			t.Error("invalid SOCKS5 URL was accepted or its diagnostic contains credentials")
		}
	}
	if _, err := socks5Dialer("socks5://[::1]:1080"); err != nil {
		t.Fatal(err)
	}
}

func TestReadSecretLinePreservesSignificantSpaceAndEOF(t *testing.T) {
	for _, input := range []string{" p a s s ", " p a s s \r\n", " p a s s \n"} {
		actual, err := readSecretLine(strings.NewReader(input))
		if err != nil || actual != " p a s s " {
			t.Errorf("readSecretLine(%q) = %q, %v", input, actual, err)
		}
	}
	if _, err := readSecretLine(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Fatalf("empty input: %v", err)
	}
}

func TestPipedCodeAndPasswordShareReader(t *testing.T) {
	input := bufio.NewReader(strings.NewReader("1234\n  secret  "))
	term := terminal.New(input, io.Discard)
	code, err := term.Code(context.Background(), &tg.AuthSentCode{Type: &tg.AuthSentCodeTypeApp{Length: 4}})
	if err != nil || code != "1234" {
		t.Fatalf("Code = %q, %v", code, err)
	}
	if password, err := readSecretLine(input); err != nil || password != "  secret  " {
		t.Fatalf("password = %q, %v", password, err)
	}
}
