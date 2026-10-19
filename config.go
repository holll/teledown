package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

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
	apiID, err := strconv.Atoi(apiIDRaw)
	if err != nil {
		return nil, fmt.Errorf("配置项 api_id 需要为整数，当前为 %q", apiIDRaw)
	}

	cfg := &Config{
		APIID:    apiID,
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

// SessionName 计算会话文件名用的账号标识（md5 前 16 位），与 Python 版同一套算法。
// 文件名与 Python 版不同（16 位 vs 32 位），避免两边同时运行时互相覆盖。
func (c *Config) SessionName() string {
	seed := c.Phone
	if seed == "" {
		seed = c.BotToken
	}
	return tools.SessionID(seed)
}

// SessionPath 是会话文件的路径：Telethon 风格的 SQLite 库，auth key 与 peer 的
// access_hash 都在里面（实现见 tools/sessionstore.go），格式与 Python 版的 .session 通用。
func (c *Config) SessionPath() string {
	return c.SessionName() + ".session"
}

// LegacySessionPath / LegacyPeerStoragePath 是合并成 SQLite 之前用的两个 JSON 文件，
// 只在首次运行时由 tools.SessionStore.ImportLegacy 读一次；旧文件不会自动删除。
func (c *Config) LegacySessionPath() string {
	return c.SessionName() + ".gotd.json"
}

func (c *Config) LegacyPeerStoragePath() string {
	return c.SessionName() + ".peers.json"
}

// parseEnvFile 解析 KEY=VALUE 形式的环境文件。
// 忽略空行与 # 注释，键统一转小写，值去掉成对的引号。
func parseEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s 不存在或无法读取: %w", path, err)
	}

	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// 与 python-dotenv 一致：没有等号的行不产生配置项
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			out[key] = value
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("配置文件 %s 为空或无法解析", path)
	}
	return out, nil
}
