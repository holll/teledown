package tools

// 本文件对应 Python 版 tools/tool.py 的通用部分：文件名规则、标题清洗、通配符、md5。

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 与 Python 版 tools/tool.py 保持一致的规则：
//   - 标题：去掉表情与 [\\/:*?"<>|]
//   - 文件名：表情替换为 [emoji]，[\\/:*?"<>|] 替换为 _
//   - 文件名为空时退回消息文本（截断到 50 字，中间省略号），再退回文件 ID
const shortenLimit = 50

var illegalChars = `\/:*?"<>|`

// sessionIDLen 会话标识长度（16 位十六进制）。
const sessionIDLen = 16

// SessionID 生成会话文件名的账号标识：md5 十六进制取前 16 位、小写。
//
// 与 Python 版 tools/tool.py 的 md5() 是同一套算法，只是截断了长度：
// 32 位在文件路径里太长，而这里只需在本机区分几个账号，16 位（64 bit）足够。
// 两版的会话文件格式本来就不通用，文件名不同不影响各自使用。
func SessionID(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:sessionIDLen]
}

// sanitizeTitle 清理频道标题。
func sanitizeTitle(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = stripEmoji(s)
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(illegalChars, r) {
			return -1
		}
		return r
	}, s)
}

// sanitizeFilename 清理由消息文本推导出的文件名。
func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = replaceEmoji(s, "[emoji]")
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(illegalChars, r) {
			return '_'
		}
		return r
	}, s)
}

// shorten 中间省略的超长文件名，与 Python 版 shorten_filename 一致。
func shorten(name string, limit int) string {
	if utf8.RuneCountInString(name) <= limit {
		return name
	}
	r := []rune(name)
	half := limit/2 - 3
	if half <= 0 {
		return string(r[:limit])
	}
	return string(r[:half]) + "..." + string(r[len(r)-limit/2:])
}

// stripEmoji 去掉表情符号。
func stripEmoji(s string) string { return replaceEmoji(s, "") }

// replaceEmoji 把表情符号替换成指定文本。
// 这里只覆盖常见区段，与 demoji 的全量表相比是近似实现。
func replaceEmoji(s, repl string) string {
	var b strings.Builder
	for _, r := range s {
		if isEmoji(r) {
			b.WriteString(repl)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // 各类表情、符号、补充符号
		return true
	case r >= 0x1F000 && r <= 0x1F2FF: // 麻将、扑克、区域符号等
		return true
	case r >= 0x2600 && r <= 0x27BF: // 杂项符号与装饰符号
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // 杂项符号与箭头
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // 变体选择符
		return true
	case r == 0x200D: // 零宽连接符
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // 区域指示符（国旗）
		return true
	case r >= 0x2190 && r <= 0x21FF: // 箭头（Telegram 中常用作装饰）
		return false
	}
	return unicode.Is(unicode.So, r) && r >= 0x2000
}

// mimeExtensions 常见 MIME 到扩展名的映射。
//
// Python 版用 Telethon 的 message.file.ext（内部是 mimetypes.guess_extension，
// Windows 上还会查注册表），表比这里大得多；gotd 不提供该字段，这里按 Telegram
// 实际会下发的 mime_type 逐个补齐，仍然识别不出时退回 .bin
// （Python 此时拿到 None，会在拼接文件名时报 TypeError，所以退回 .bin 更稳）。
var mimeExtensions = map[string]string{
	// 视频
	"video/mp4":        ".mp4",
	"video/x-matroska": ".mkv",
	"video/webm":       ".webm",
	"video/quicktime":  ".mov",
	"video/x-msvideo":  ".avi",
	"video/avi":        ".avi",
	"video/x-ms-wmv":   ".wmv",
	"video/x-flv":      ".flv",
	"video/mpeg":       ".mpeg",
	"video/3gpp":       ".3gp",
	"video/mp2t":       ".ts",
	"video/x-m4v":      ".m4v",
	// 图片
	"image/jpeg":    ".jpg",
	"image/png":     ".png",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/bmp":     ".bmp",
	"image/tiff":    ".tiff",
	"image/heic":    ".heic",
	"image/svg+xml": ".svg",
	"image/avif":    ".avif",
	// 音频
	"audio/mpeg":   ".mp3",
	"audio/mp4":    ".m4a",
	"audio/x-m4a":  ".m4a",
	"audio/ogg":    ".ogg",
	"audio/opus":   ".opus",
	"audio/flac":   ".flac",
	"audio/x-flac": ".flac",
	"audio/wav":    ".wav",
	"audio/x-wav":  ".wav",
	"audio/aac":    ".aac",
	"audio/webm":   ".weba",
	"audio/amr":    ".amr",
	// 压缩包与可执行文件
	"application/zip":                               ".zip",
	"application/x-7z-compressed":                   ".7z",
	"application/x-rar-compressed":                  ".rar",
	"application/vnd.rar":                           ".rar",
	"application/x-tar":                             ".tar",
	"application/gzip":                              ".gz",
	"application/x-gzip":                            ".gz",
	"application/x-bzip2":                           ".bz2",
	"application/x-xz":                              ".xz",
	"application/x-iso9660-image":                   ".iso",
	"application/x-msdownload":                      ".exe",
	"application/vnd.microsoft.portable-executable": ".exe",
	"application/vnd.android.package-archive":       ".apk",
	"application/java-archive":                      ".jar",
	"application/vnd.apple.installer+xml":           ".mpkg",
	"application/x-apple-diskimage":                 ".dmg",
	"application/x-deb":                             ".deb",
	"application/x-rpm":                             ".rpm",
	"application/x-msi":                             ".msi",
	// 文档
	"application/pdf":               ".pdf",
	"application/epub+zip":          ".epub",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/rtf":          ".rtf",
	"application/json":         ".json",
	"application/xml":          ".xml",
	"application/x-yaml":       ".yaml",
	"application/x-subrip":     ".srt",
	"application/x-ass":        ".ass",
	"text/plain":               ".txt",
	"text/csv":                 ".csv",
	"text/html":                ".html",
	"text/css":                 ".css",
	"text/javascript":          ".js",
	"text/x-python":            ".py",
	"text/markdown":            ".md",
	"application/x-sh":         ".sh",
	"application/x-bittorrent": ".torrent",
}

// ExtensionForMIME 由 MIME 推测扩展名，未知时退回 .bin。
func ExtensionForMIME(mime string) string {
	if ext, ok := mimeExtensions[strings.ToLower(strings.TrimSpace(mime))]; ok {
		return ext
	}
	return ".bin"
}

// normalizeExt 统一 JPEG 变体后缀，与 Python 版一致。
func normalizeExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpe", ".jpeg":
		return ".jpg"
	}
	return ext
}

// shortenFilename 由消息文本推导文件名，处理顺序与 Python 版 get_file_name 一致：
// 表情替换为 [emoji]、换行替换为空格 → 中间省略 → 非法字符替换。
// 顺序会影响结果：Python 是先做 emoji 替换再截断，反过来（先截断）会让含表情或
// 换行的长描述截断位置与 Python 版不一致。
func shortenFilename(s string, limit int) string {
	s = strings.ReplaceAll(replaceEmoji(s, "[emoji]"), "\n", " ")
	return sanitizeFilename(shorten(s, limit))
}

// BuildFilename 按 Python 版优先级生成文件名：
// 原始文件名 > 消息文本（截断后）> 文件 ID，再接扩展名。
func BuildFilename(originalName, messageText string, fileID int64, mime string) string {
	if originalName != "" {
		return sanitizeFilename(originalName)
	}

	ext := ExtensionForMIME(mime)
	if messageText != "" {
		return shortenFilename(messageText, shortenLimit) + ext
	}
	return fmt.Sprintf("%d%s", fileID, ext)
}

// MonthDir 由消息时间戳生成按月分层的目录名（形如 "2026-08"）。
//
// 下载时用它把媒体归档到「频道/年-月」子目录。Telegram 的 Date 是 UTC 秒级
// 时间戳，这里转成本地时区再取年月，归档月份才符合使用者所在时区的直觉。
func MonthDir(unix int) string {
	return time.Unix(int64(unix), 0).Format("2006-01")
}

// ResolveSavePath 处理重名文件：
// 同名且大小相同视为已下载（返回 true）；否则依次尝试 name(2).ext、name(3).ext。
func ResolveSavePath(path string, size int64) (exists bool, target string) {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)

	target = path
	for i := 2; ; i++ {
		info, err := os.Stat(target)
		if err != nil {
			return false, target
		}
		if size > 0 && info.Size() == size {
			return true, target
		}
		target = fmt.Sprintf("%s(%d)%s", base, i, ext)
	}
}

// wildcardFoldCase 决定通配符匹配是否忽略大小写。
//
// Python 版用的 fnmatch.fnmatch 会先对两侧做 os.path.normcase，而 normcase 在
// Windows 上是「斜杠转反斜杠 + 转小写」，在其它平台是恒等函数。也就是说同一条
// --prefix "*.MP4"，Python 版在 Windows 上能匹配 a.mp4、在 Linux 上不能。
// Go 的 filepath.Match 两个平台都区分大小写，这里补上 Windows 的这层差异。
var wildcardFoldCase = runtime.GOOS == "windows"

// MatchWildcard 支持 ; 分隔的多条通配符，pattern 为空时全部匹配。
func MatchWildcard(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if wildcardFoldCase {
		name = strings.ToLower(name)
	}
	for _, p := range strings.Split(pattern, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if wildcardFoldCase {
			p = strings.ToLower(p)
		}
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}
