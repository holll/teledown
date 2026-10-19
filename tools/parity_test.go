package tools

import (
	"strings"
	"testing"
)

// 本文件用 Python 版原函数的实际输出作为基准，验证两侧行为一致。
// 基准数据由下面这段 Python 生成（对应 tools/tool.py 的 shorten_filename / match_wildcard）：
//
//	shorten_filename('a'*60)
//	shorten_filename('中文'*40)
func TestShortenParityWithPython(t *testing.T) {
	a60 := strings.Repeat("a", 60)
	cn40 := strings.Repeat("中文", 40)

	cases := []struct{ in, want string }{
		{"短.mp4", "短.mp4"},
		{"第01集 上.mp4", "第01集 上.mp4"},
		{a60, "aaaaaaaaaaaaaaaaaaaaaa...aaaaaaaaaaaaaaaaaaaaaaaaa"},
		{cn40, "中文中文中文中文中文中文中文中文中文中文中文...文中文中文中文中文中文中文中文中文中文中文中文中文"},
	}
	for _, c := range cases {
		if got := shorten(c.in, shortenLimit); got != c.want {
			t.Errorf("shorten(%q)\n 得到 %q\n 期望 %q", c.in, got, c.want)
		}
	}
}

func TestWildcardParityWithPython(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.mp4", "a.mp4", true},
		{"*.mp4", "a.mkv", false},
		{"*.mp4;*.mkv", "a.mkv", true},
		{"第*集*", "第01集 上.mp4", true},
		{"不匹配*", "别的.mp4", false},
		// 刻意差异：Python 里 pattern="" 会走 fnmatch 并返回 False，
		// 而 Go 的 flag 默认值就是空串，无法区分「未指定」与「显式传空」，
		// 因此把空串定义为「全部匹配」（等价于 Python 的 pattern=None）。
		{"", "任意.mkv", true},
	}
	for _, c := range cases {
		if got := MatchWildcard(c.pattern, c.name); got != c.want {
			t.Errorf("MatchWildcard(%q, %q) = %v, 期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}
