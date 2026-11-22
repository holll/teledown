package tools

// 本文件对应 Python 版 tools/upload_file.py：上传流程。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// UploadOptions 是上传命令的参数。
type UploadOptions struct {
	Path        string
	DeleteAfter bool
	Tag         string
	Threads     int
}

// Upload 上传单个文件或整个目录到目标会话。
func Upload(ctx context.Context, api *tg.Client, peer *Peer, opt UploadOptions) error {
	files, isDir, err := collectFiles(opt.Path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("路径 %s 下没有可上传的文件", opt.Path)
	}

	var failed int
	for _, path := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := uploadOne(ctx, api, peer.Input, path, opt); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Printf("上传出错 %s: %v\n", filepath.Base(path), err)
			failed++
			continue
		}
	}

	if isDir && failed == 0 {
		// 与 Python 版一致：整目录上传完成后，若目录已空则删除
		if entries, err := os.ReadDir(opt.Path); err == nil && len(entries) == 0 {
			_ = os.Remove(opt.Path)
		}
	}
	if failed > 0 {
		return fmt.Errorf("有 %d 个文件上传失败", failed)
	}
	return nil
}

func uploadOne(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, path string, opt UploadOptions) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("获取上传文件的绝对路径: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取文件信息失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("不上传非普通文件: %s", path)
	}
	name := filepath.Base(path)
	mimeType := detectMIME(path)

	var videoAttr *tg.DocumentAttributeVideo
	isVideo := strings.HasPrefix(mimeType, "video/")
	if isVideo {
		videoAttr = probeVideoAttributes(ctx, path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if videoAttr == nil {
			fmt.Printf("提示：未能读取视频元信息（请检查 ffprobe 与视频文件），%s 将以普通文件上传\n", name)
		}
	}

	var thumbData []byte
	if videoAttr != nil {
		thumb, err := ExtractThumbnail(ctx, path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err != nil:
			// 缩略图属于锦上添花，失败不阻断上传
			fmt.Printf("提示：%s 未能生成缩略图（%v），继续上传\n", name, err)
		case !IsJPEG(thumb):
			fmt.Printf("提示：%s 生成的缩略图不是有效 JPEG，已忽略\n", name)
		default:
			thumbData = thumb
		}
	}

	bar := newProgressBar(name, info.Size())
	// 与下载一致：失败时只清掉进行中的一行，成功时留一条汇总行
	finished := false
	defer func() {
		if !finished {
			bar.Finish(false)
		}
	}()

	up := uploader.NewUploader(api).WithThreads(opt.Threads).WithProgress(bar)
	// 用 FromPath 而不是自己 os.Open：它内部负责关闭句柄，
	// 而 Windows 上句柄未关闭时 os.Remove 一定会失败（-dau Y 会删不掉源文件）。
	uploaded, err := up.FromPath(ctx, path)
	if err != nil {
		return fmt.Errorf("上传失败: %w", err)
	}

	var thumbInput tg.InputFileClass
	if len(thumbData) > 0 {
		// 缩略图很小，不再单独显示进度
		input, err := uploader.NewUploader(api).FromBytes(ctx, "thumb.jpg", thumbData)
		if err != nil {
			fmt.Printf("提示：%s 缩略图上传失败（%v），改以无缩略图上传\n", name, err)
		} else {
			thumbInput = input
		}
	}

	caption := strings.TrimSuffix(name, filepath.Ext(name))
	if opt.Tag != "" {
		caption = "#" + strings.TrimPrefix(opt.Tag, "#") + " " + caption
	}

	// 交给 message.Sender 发送：附件的 MimeType / Attributes / Thumb 组装和
	// random_id 生成（RandomID 为 0 时自动补）都由它负责，不必手拼请求。
	doc := message.UploadedDocument(uploaded, styling.Plain(caption)).
		Filename(name).
		MIME(mimeType).
		ForceFile(isVideo && videoAttr == nil)
	if videoAttr != nil {
		doc = doc.Attributes(videoAttr)
	}
	if thumbInput != nil {
		doc = doc.Thumb(thumbInput)
	}
	if _, err := message.NewSender(api).To(peer).Media(ctx, doc); err != nil {
		return fmt.Errorf("发送媒体消息失败: %w", err)
	}

	// 附件已经发出去，进度条到此收尾；后面删源文件失败不属于上传失败
	bar.Finish(true)
	finished = true
	if opt.DeleteAfter {
		if err := os.Remove(path); err != nil {
			fmt.Printf("提示：删除源文件失败 %s: %v\n", path, err)
		}
	}
	return nil
}

// collectFiles 收集待上传文件，目录递归收集并按路径排序（与 Python 版一致）。
func collectFiles(path string) (files []string, isDir bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, fmt.Errorf("路径 %s 不存在或无法访问: %w", path, err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("不上传非普通文件: %s", path)
		}
		return []string{path}, false, nil
	}

	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, true, fmt.Errorf("遍历目录失败: %w", err)
	}
	slices.Sort(files)
	return files, true, nil
}

// detectMIME 优先按扩展名判断，识别不出时退回通用二进制类型。
func detectMIME(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		base, _, _ := strings.Cut(t, ";")
		return strings.TrimSpace(base)
	}
	return "application/octet-stream"
}

// ffprobeResult 只取需要的字段。
type ffprobeResult struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Duration    string `json:"duration"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// probeVideoAttributes 用 ffprobe 读取时长与分辨率，生成视频属性。
// 未安装 ffprobe 或读取失败时返回 nil，调用方按普通文件上传。
func probeVideoAttributes(ctx context.Context, path string) *tg.DocumentAttributeVideo {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}

	var result ffprobeResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return nil
	}

	return videoAttributes(result)
}

func videoAttributes(result ffprobeResult) *tg.DocumentAttributeVideo {
	for _, s := range result.Streams {
		if s.CodecType != "video" || s.Disposition.AttachedPic != 0 || s.Width <= 0 || s.Height <= 0 {
			continue
		}
		duration := positiveDuration(s.Duration)
		if duration == 0 {
			duration = positiveDuration(result.Format.Duration)
		}
		if duration > 0 {
			return &tg.DocumentAttributeVideo{
				Duration:          duration,
				W:                 s.Width,
				H:                 s.Height,
				SupportsStreaming: true,
			}
		}
	}
	return nil
}

func positiveDuration(raw string) float64 {
	duration, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return 0
	}
	return duration
}
