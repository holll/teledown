package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSanitizeTitleAndFilename(t *testing.T) {
	cases := []struct {
		in       string
		title    string
		filename string
	}{
		// 标题：表情与非法字符直接删除；文件名：表情替换为 [emoji]、非法字符替换为 _
		{in: "电影🎬: 第一部?", title: "电影 第一部", filename: "电影[emoji]_ 第一部_"},
		{in: `a/b\c:d*e?f"g<h>i|j`, title: "abcdefghij", filename: "a_b_c_d_e_f_g_h_i_j"},
		{in: "普通标题", title: "普通标题", filename: "普通标题"},
		{in: "多行\n标题", title: "多行 标题", filename: "多行 标题"},
	}
	for _, c := range cases {
		if got := sanitizeTitle(c.in); got != c.title {
			t.Errorf("sanitizeTitle(%q) = %q, 期望 %q", c.in, got, c.title)
		}
		if got := sanitizeFilename(c.in); got != c.filename {
			t.Errorf("sanitizeFilename(%q) = %q, 期望 %q", c.in, got, c.filename)
		}
	}
}

func TestShortenKeepsShortInput(t *testing.T) {
	short := "短文件名.mp4"
	if got := shorten(short, shortenLimit); got != short {
		t.Errorf("短文件名不应被截断，得到 %q", got)
	}

	long := ""
	for i := 0; i < 100; i++ {
		long += "字"
	}
	got := shorten(long, shortenLimit)
	runes := []rune(got)
	if len(runes) != shortenLimit {
		t.Errorf("截断后长度 = %d，期望 %d", len(runes), shortenLimit)
	}
	// 与 Python 版一致：前 22 字 + ... + 末 25 字
	if string(runes[22:25]) != "..." {
		t.Errorf("截断后应在第 22 字处出现省略号，得到 %q", got)
	}
}

func TestBuildFilenamePriority(t *testing.T) {
	// 1) 原始文件名优先
	if got := BuildFilename("原文件名.mkv", "消息文本", 999, "video/x-matroska"); got != "原文件名.mkv" {
		t.Errorf("应优先使用原始文件名，得到 %q", got)
	}
	// 2) 无原始名时用消息文本 + MIME 推导的扩展名
	if got := BuildFilename("", "一段描述", 999, "video/mp4"); got != "一段描述.mp4" {
		t.Errorf("应由消息文本推导文件名，得到 %q", got)
	}
	// 3) 都没有时用文件 ID
	if got := BuildFilename("", "", 12345, "image/jpeg"); got != "12345.jpg" {
		t.Errorf("应回退到文件 ID，得到 %q", got)
	}
	// 4) JPEG 变体后缀归一
	if got := normalizeExt(".jpeg"); got != ".jpg" {
		t.Errorf("jpeg 应归一为 .jpg，得到 %q", got)
	}
	// 5) 未知 MIME 回退 .bin
	if got := ExtensionForMIME("application/x-unknown"); got != ".bin" {
		t.Errorf("未知 MIME 应回退 .bin，得到 %q", got)
	}
}

func TestResolveSavePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "video.mp4")

	// 文件不存在
	if exists, got := ResolveSavePath(target, 100); exists || got != target {
		t.Errorf("文件不存在时应返回原路径，得到 exists=%v path=%q", exists, got)
	}

	// 同名同大小 -> 视为已下载
	if err := os.WriteFile(target, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if exists, _ := ResolveSavePath(target, 100); !exists {
		t.Error("同名同大小应判定为已存在")
	}

	// 同名不同大小 -> 加序号
	if exists, got := ResolveSavePath(target, 200); exists || got != filepath.Join(dir, "video(2).mp4") {
		t.Errorf("同名不同大小应加序号，得到 exists=%v path=%q", exists, got)
	}

	// (2) 也被占用且大小不同 -> (3)
	if err := os.WriteFile(filepath.Join(dir, "video(2).mp4"), make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	if exists, got := ResolveSavePath(target, 200); exists || got != filepath.Join(dir, "video(3).mp4") {
		t.Errorf("应继续顺延到 (3)，得到 exists=%v path=%q", exists, got)
	}

	// (2) 大小相同 -> 判定已存在
	if exists, _ := ResolveSavePath(target, 50); !exists {
		t.Error("序号文件大小相同应判定为已存在")
	}
}

func TestMatchWildcard(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"", "任意.mp4", true}, // 未指定前缀时全部匹配
		{"*.mp4", "a.mp4", true},
		{"*.mp4", "a.mkv", false},
		{"*.mp4;*.mkv", "a.mkv", true}, // ; 分隔多条
		{"*.mp4; *.mkv", "a.mkv", true},
		{"第*集*", "第01集 上.mp4", true},
		{"不匹配*", "别的.mp4", false},
	}
	for _, c := range cases {
		if got := MatchWildcard(c.pattern, c.name); got != c.want {
			t.Errorf("MatchWildcard(%q, %q) = %v, 期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}

// Python 的 fnmatch.fnmatch 会先做 os.path.normcase，而 normcase 在 Windows 上转小写，
// 所以同一份 --prefix 在 Windows 上能匹配不同大小写的文件名，在其它平台不能。
func TestMatchWildcardCaseFolding(t *testing.T) {
	cases := []struct{ pattern, name string }{
		{"*.MP4", "a.mp4"},
		{"*.mp4", "A.MP4"},
		{"第*集*", "第01集 上.MP4"},
	}
	for _, c := range cases {
		if runtime.GOOS == "windows" {
			if !MatchWildcard(c.pattern, c.name) {
				t.Errorf("Windows 下 MatchWildcard(%q, %q) 应忽略大小写", c.pattern, c.name)
			}
			continue
		}
		if MatchWildcard(c.pattern, c.name) {
			t.Errorf("非 Windows 平台 MatchWildcard(%q, %q) 应区分大小写", c.pattern, c.name)
		}
	}
}

// 由消息文本推导文件名时的处理顺序：表情与换行要在截断之前处理完（与 Python 版一致）。
func TestBuildFilenameNormalizesBeforeShortening(t *testing.T) {
	// 60 个表情：先替换成 [emoji] 再截断，结果长度仍是 50，且不含原始表情
	got := BuildFilename("", strings.Repeat("🎬", 60), 1, "video/mp4")
	if n := len([]rune(got)); n != shortenLimit+len(".mp4") {
		t.Errorf("文件名长度 = %d，期望 %d（截断在 emoji 替换之后）", n, shortenLimit+len(".mp4"))
	}
	if !strings.Contains(got, "[emoji]") {
		t.Errorf("表情应被替换为 [emoji]，得到 %q", got)
	}

	// 含换行的长文本：换行必须先变成空格，且截断结果里不能残留换行
	got = BuildFilename("", "第一行\n"+strings.Repeat("字", 60), 1, "text/plain")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("文件名不应含换行符，得到 %q", got)
	}
	if !strings.HasSuffix(got, ".txt") {
		t.Errorf("应保留扩展名，得到 %q", got)
	}
}

func TestExtensionForMIMEExpandedTable(t *testing.T) {
	cases := map[string]string{
		"video/mp4":                      ".mp4",
		"video/x-matroska":               ".mkv",
		"audio/x-flac":                   ".flac",
		"application/x-msdownload":       ".exe",
		"application/zip":                ".zip",
		"application/x-rar-compressed":   ".rar",
		"application/x-subrip":           ".srt",
		"text/x-python":                  ".py",
		"IMAGE/JPEG":                     ".jpg", // 大小写无关
		"application/x-unknown-vendor":   ".bin", // 未收录仍退回 .bin
		"application/x-tgsticker-custom": ".bin",
	}
	for mimeType, want := range cases {
		if got := ExtensionForMIME(mimeType); got != want {
			t.Errorf("ExtensionForMIME(%q) = %q，期望 %q", mimeType, got, want)
		}
	}
	// Office 文档是这次扩容的重点：以前会退化成 12345.bin
	for _, mimeType := range []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/msword",
	} {
		if got := ExtensionForMIME(mimeType); got == ".bin" {
			t.Errorf("ExtensionForMIME(%q) 不应退化成 .bin", mimeType)
		}
	}
}

func TestMonthDir(t *testing.T) {
	// 用本地时区构造时间戳，避免测试机时区影响跨月边界
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 8, 15, 12, 0, 0, 0, time.Local), "2026-08"},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local), "2026-01"},
		{time.Date(2025, 12, 31, 23, 59, 59, 0, time.Local), "2025-12"},
		{time.Date(2000, 3, 5, 8, 30, 0, 0, time.Local), "2000-03"},
	}
	for _, c := range cases {
		if got := MonthDir(int(c.t.Unix())); got != c.want {
			t.Errorf("MonthDir(%v) = %q，期望 %q", c.t, got, c.want)
		}
	}
}
