package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/contrib/auth/terminal"
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

// 会话标识取 Python 版 md5(phone or bot_token) 的前 16 位。
// 下面注释里的 32 位值是用 Python 版 tools/tool.py 的 md5() 实测得到的。
func TestSessionNameIsPythonMD5Prefix(t *testing.T) {
	cases := []struct {
		name      string
		cfg       Config
		pythonMD5 string // Python 版 md5() 的完整输出
	}{
		// md5("13800138000")
		{"手机号登录", Config{Phone: "13800138000"}, "7945bd83237335e5376ff44d62e4f0ae"},
		// md5("123:abc")
		{"机器人登录", Config{BotToken: "123:abc"}, "ebecf09cd7c661306f05c7c7fa017549"},
	}
	for _, c := range cases {
		got := c.cfg.SessionName()
		if len(got) != 16 {
			t.Errorf("%s: SessionName() = %q，长度应为 16", c.name, got)
		}
		if want := c.pythonMD5[:16]; got != want {
			t.Errorf("%s: SessionName() = %q，期望是 Python md5 的前 16 位 %q", c.name, got, want)
		}
	}
}
