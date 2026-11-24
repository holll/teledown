package tools

// 本文件对应 Python 版 tools/upload_file.py：上传流程。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
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
	Premium     bool
}

const (
	defaultUploadParts = 4000
	premiumUploadParts = 8000
)

type uploadConfigClient interface {
	HelpGetAppConfig(context.Context, int) (tg.HelpAppConfigClass, error)
}

func uploadPartsLimit(ctx context.Context, client uploadConfigClient, premium bool) int {
	key, fallback := "upload_max_fileparts_default", defaultUploadParts
	if premium {
		key, fallback = "upload_max_fileparts_premium", premiumUploadParts
	}
	// 一次查询覆盖本批次；不让配置 RPC（如 bot 无权限或限流）卡住上传。
	configCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := client.HelpGetAppConfig(configCtx, 0)
	config, ok := resp.(*tg.HelpAppConfig)
	if err != nil || !ok || config == nil {
		return fallback
	}
	obj, ok := config.Config.(*tg.JSONObject)
	if !ok || obj == nil {
		return fallback
	}
	for _, entry := range obj.Value {
		if entry.Key != key {
			continue
		}
		if n, ok := entry.Value.(*tg.JSONNumber); ok && n != nil && n.Value >= 1 && n.Value <= math.MaxInt32 && math.Trunc(n.Value) == n.Value {
			return int(n.Value)
		}
		break
	}
	return fallback
}

func uploadPartSize(size int64, maxParts int, premium bool) (int, error) {
	maxBytes := int64(maxParts) * uploader.MaximumPartSize
	if size > maxBytes {
		tier := "普通账号"
		if premium {
			tier = "Premium 账号"
		}
		return 0, fmt.Errorf("%s：文件大小 %d 字节超过单文件上限 %d 字节（最多 %d 片）", tier, size, maxBytes, maxParts)
	}
	// Keep gotd's 128 KiB baseline, but grow using the active tier's limit.
	// Explicit WithPartSize makes the preflight and actual part count agree.
	partSize := 128 * 1024
	for size > int64(maxParts)*int64(partSize) {
		partSize *= 2
	}
	return partSize, nil
}

type uploadPartRetryClient struct {
	uploader.Client
}

func (c uploadPartRetryClient) UploadSaveFilePart(ctx context.Context, req *tg.UploadSaveFilePartRequest) (bool, error) {
	return retryUploadPart(ctx, func() (bool, error) { return c.Client.UploadSaveFilePart(ctx, req) })
}

func (c uploadPartRetryClient) UploadSaveBigFilePart(ctx context.Context, req *tg.UploadSaveBigFilePartRequest) (bool, error) {
	return retryUploadPart(ctx, func() (bool, error) { return c.Client.UploadSaveBigFilePart(ctx, req) })
}

func retryUploadPart(ctx context.Context, send func() (bool, error)) (bool, error) {
	const maxRetries = 2
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ok, err := send()
		if err == nil && ok {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// gotd uses a wrapped context.Canceled for confirmed requests without a
		// result on engine shutdown. Its upload loops also retry false ACKs
		// indefinitely, so consume those here and return a terminal error at the cap.
		if err == nil {
			err = errors.New("Telegram 未确认保存该分片")
		} else if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "engine forcibly closed:") {
			return false, err
		}
		if attempt == maxRetries {
			return false, err
		}
		timer := time.NewTimer(time.Second << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
			TransferStatsFrom(ctx).RecordUploadRetry()
		}
	}
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

	maxParts := uploadPartsLimit(ctx, api, opt.Premium)
	var failed int
	for _, path := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := uploadOne(ctx, api, peer.Input, path, opt, maxParts); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !isDir {
				return fmt.Errorf("上传 %s 失败: %w", filepath.Base(path), err)
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

func uploadOne(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, path string, opt UploadOptions, maxParts int) error {
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
	partSize, err := uploadPartSize(info.Size(), maxParts, opt.Premium)
	if err != nil {
		return err
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

	partClient := uploadPartRetryClient{Client: api}
	up := uploader.NewUploader(partClient).WithThreads(opt.Threads).WithProgress(bar).WithPartSize(partSize)
	uploaded, err := uploadFileBytes(ctx, up, path, info)
	if err != nil {
		return fmt.Errorf("上传文件分片失败: %w", err)
	}

	var thumbInput tg.InputFileClass
	if len(thumbData) > 0 {
		// 缩略图很小，不再单独显示进度
		input, err := uploader.NewUploader(partClient).FromBytes(ctx, "thumb.jpg", thumbData)
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
	// 文件分片已上传；提交失败直接返回，不重传文件，也不生成新随机 ID 重发消息。
	if _, err := message.NewSender(api).To(peer).Media(ctx, doc); err != nil {
		TransferStatsFrom(ctx).RecordMediaSendFailure()
		return fmt.Errorf("文件分片已上传，但发送媒体消息失败: %w", err)
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

func uploadFileBytes(ctx context.Context, up *uploader.Uploader, path string, expected fs.FileInfo) (uploaded tg.InputFileClass, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(expected, info) || expected.Size() != info.Size() {
		return nil, errors.New("待上传文件在预检后发生变化，请重新执行")
	}
	// Use exactly the size checked above instead of recomputing it in FromPath.
	reader := &io.LimitedReader{R: f, N: info.Size()}
	uploaded, err = up.Upload(ctx, uploader.NewUpload(info.Name(), reader, info.Size()))
	if err == nil && reader.N != 0 {
		return nil, fmt.Errorf("待上传文件提前结束，少读取 %d 字节", reader.N)
	}
	return uploaded, err
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

// detectMIME 识别文件头；无法区分格式时，优先保留兼容的扩展名类型。
func detectMIME(path string) string {
	const binary = "application/octet-stream"
	ext := strings.ToLower(filepath.Ext(path))
	byExt := baseMIME(mime.TypeByExtension(ext))
	if byExt == "" {
		byExt = binary
	}
	f, err := os.Open(path)
	if err != nil {
		return byExt
	}
	defer f.Close()

	var header [512]byte
	n, err := io.ReadFull(f, header[:])
	if n == 0 || (err != nil && err != io.ErrUnexpectedEOF) {
		return byExt
	}
	byHeader := baseMIME(http.DetectContentType(header[:n]))
	switch byHeader {
	case binary:
		return byExt
	case "text/plain":
		switch ext {
		case ".csv":
			// Windows 注册表可能把 CSV 映射到旧版 Excel 工作簿类型。
			return "text/csv"
		case ".srt":
			return "application/x-subrip"
		}
		if byExt != binary {
			return byExt
		}
	case "text/xml":
		if strings.HasSuffix(byExt, "+xml") || byExt == "application/xml" {
			return byExt
		}
	case "application/zip":
		if strings.HasSuffix(byExt, "+zip") || strings.HasPrefix(byExt, "application/vnd.openxmlformats-officedocument.") ||
			strings.HasSuffix(byExt, ".macroenabled.12") || byExt == "application/vnd.android.package-archive" || byExt == "application/java-archive" {
			return byExt
		}
	case "application/ogg":
		if strings.HasSuffix(byExt, "/ogg") || byExt == "audio/opus" {
			return byExt
		}
	case "video/mp4":
		switch byExt {
		case "audio/mp4", "audio/x-m4a", "video/x-m4v", "video/quicktime":
			return byExt
		}
	case "video/webm":
		// 标准库只检查 EBML 魔数，无法区分 WebM 和 Matroska。
		switch ext {
		case ".webm":
			return "video/webm"
		case ".weba":
			return "audio/webm"
		case ".mkv":
			return "video/x-matroska"
		case ".mka":
			return "audio/x-matroska"
		default:
			return binary
		}
	}
	return byHeader
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
