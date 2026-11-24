package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

	"teledown-go/tools"
)

// Config 对应 Python 版的 .env 配置。
// 键名不区分大小写（与 Python 版一致：读取后统一转小写）。
type Config struct {
	APIID    int
	APIHash  string
	Phone    string
	Password string // 两步验证密码，可留空
	BotToken string
	SavePath string
	Proxy    string
}

// LoadConfig 读取 .env 并解析必需项。
func LoadConfig(path string) (*Config, error) {
	raw, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}

	apiIDRaw := raw["api_id"]
	if apiIDRaw == "" || raw["api_hash"] == "" {
		return nil, fmt.Errorf("请在 %s 中填写 api_id 与 api_hash", path)
	}
	apiID, err := strconv.ParseInt(apiIDRaw, 10, 32)
	if err != nil || apiID <= 0 {
		return nil, fmt.Errorf("配置项 api_id 需要为正的 32 位整数，当前为 %q", apiIDRaw)
	}

	cfg := &Config{
		APIID:    int(apiID),
		APIHash:  raw["api_hash"],
		Phone:    raw["phone"],
		Password: raw["password"],
		BotToken: raw["bot_token"],
		SavePath: raw["save_path"],
		Proxy:    raw["proxy"],
	}

	switch {
	case cfg.Phone != "" && cfg.BotToken != "":
		return nil, fmt.Errorf("phone 与 bot_token 只能填写一个")
	case cfg.Phone == "" && cfg.BotToken == "":
		return nil, fmt.Errorf("请在 %s 中填写 phone 或 bot_token", path)
	}
	return cfg, nil
}

// SessionName 计算会话文件名用的稳定账号标识（md5 前 16 位）。
func (c *Config) SessionName() string {
	seed := c.Phone
	if seed == "" {
		seed = c.BotToken
	}
	return tools.SessionID(seed)
}

// SessionPath 返回 Go 版专用会话库的路径，与旧版 SQLite 和 JSON 文件名隔离。
func (c *Config) SessionPath() string {
	return c.SessionName() + ".teledown.sqlite3"
}

// parseEnvFile 解析 KEY=VALUE 形式的环境文件。
// 忽略空行与 # 注释，键统一转小写，值去掉成对的引号。
func parseEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s 不存在或无法读取: %w", path, err)
	}

	out := make(map[string]string)
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// 与 python-dotenv 一致：没有等号的行不产生配置项
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			out[key] = parseEnvValue(value)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("配置文件 %s 为空或无法解析", path)
	}
	return out, nil
}

// parseEnvValue preserves literal hashes within quoted values and unquoted
// hashes without preceding whitespace (e.g. a password ending in "#abc").
func parseEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
		quote := value[0]
		for i := 1; i < len(value); i++ {
			if value[i] == '\\' && i+1 < len(value) && (value[i+1] == quote || value[i+1] == '\\') {
				i++
				continue
			}
			if value[i] == quote {
				tail := strings.TrimSpace(value[i+1:])
				if tail == "" || strings.HasPrefix(tail, "#") {
					return strings.NewReplacer(`\`+string(quote), string(quote), `\\`, `\`).Replace(value[1:i])
				}
				return value
			}
		}
		return value
	}
	previousSpace := false
	for i, r := range value {
		if r == '#' && previousSpace {
			return strings.TrimSpace(value[:i])
		}
		previousSpace = unicode.IsSpace(r)
	}
	return value
}
