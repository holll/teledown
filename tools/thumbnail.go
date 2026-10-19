package tools

// 本文件对应 Python 版 tools/tool.py 的 get_thumb：视频首帧抽图。

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"os/exec"
	"time"
)

// thumbMaxWidth 是缩略图最大宽度，超过则等比缩小，不放大。
// Telegram 客户端展示缩略图不需要大图，控制体积也能省一次上传流量。
const thumbMaxWidth = 320

// thumbnailTimeout 限制单次抽帧耗时，避免异常文件把上传流程卡死。
const thumbnailTimeout = 30 * time.Second

// ExtractThumbnail 抽取视频首帧并编码为 JPEG。
// 与 Python 版 tools/tool.py 的 get_thumb 等价，只是改用 ffmpeg 一步到位，
// 不再需要 moviepy + Pillow。
func ExtractThumbnail(path string) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("未找到 ffmpeg: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), thumbnailTimeout)
	defer cancel()

	var (
		out    bytes.Buffer
		stderr bytes.Buffer
	)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", path,
		// 取第一帧；scale 用 min() 避免把小于 320 宽的视频放大
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-1", thumbMaxWidth),
		"-f", "mjpeg",
		"-",
	)
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("抽帧失败: %v: %s", err, msg)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("抽帧结果为空")
	}
	return out.Bytes(), nil
}

// IsJPEG 用于校验 ffmpeg 输出确实是可解码的 JPEG，避免把坏数据传给 Telegram。
//
// 用标准库真正解一次文件头，而不是比对首尾魔数（FF D8 / FF D9）：
// 魔数校验会把「只有 SOI 标记的垃圾数据」当成合法 JPEG，解头才能挡下这种数据。
func IsJPEG(data []byte) bool {
	_, err := jpeg.DecodeConfig(bytes.NewReader(data))
	return err == nil
}
