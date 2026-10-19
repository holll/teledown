package tools

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeTestVideo 用 ffmpeg 合成一段测试视频，避免测试依赖外部素材。
func makeTestVideo(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=640x480:rate=1",
		"-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("无法生成测试视频（ffmpeg 不可用？）: %v: %s", err, out)
	}
}

func TestExtractThumbnail(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过缩略图测试")
	}

	video := filepath.Join(t.TempDir(), "test.mp4")
	makeTestVideo(t, video)

	data, err := ExtractThumbnail(video)
	if err != nil {
		t.Fatalf("抽帧失败: %v", err)
	}
	if !IsJPEG(data) {
		t.Fatalf("输出不是有效 JPEG，前 4 字节: %x", data[:min(4, len(data))])
	}

	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("JPEG 解码失败: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != thumbMaxWidth {
		t.Errorf("宽度 = %d，期望缩放到 %d", bounds.Dx(), thumbMaxWidth)
	}
	if bounds.Dy() <= 0 {
		t.Errorf("高度异常: %d", bounds.Dy())
	}
	t.Logf("缩略图: %dx%d, %d 字节", bounds.Dx(), bounds.Dy(), len(data))
}

func TestExtractThumbnailSmallVideoNotUpscaled(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过缩略图测试")
	}

	video := filepath.Join(t.TempDir(), "small.mp4")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=1",
		"-pix_fmt", "yuv420p", video)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("无法生成测试视频: %v: %s", err, out)
	}

	data, err := ExtractThumbnail(video)
	if err != nil {
		t.Fatalf("抽帧失败: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("JPEG 解码失败: %v", err)
	}
	if got := img.Bounds().Dx(); got != 160 {
		t.Errorf("窄于 %d 的视频不应被放大，实测宽度 %d", thumbMaxWidth, got)
	}
}

func TestExtractThumbnailOnNonVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过缩略图测试")
	}

	path := filepath.Join(t.TempDir(), "not-a-video.txt")
	if err := os.WriteFile(path, []byte("这不是视频"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ExtractThumbnail(path); err == nil {
		t.Error("对非视频文件抽帧应当报错，交由调用方降级处理")
	}
}

func TestIsJPEG(t *testing.T) {
	// 先编码一张真实 JPEG 作为基准
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	valid := buf.Bytes()

	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"真实 JPEG", valid, true},
		// 只有 SOI 标记的垃圾数据：改之前的魔数校验会放过去，解头才会发现
		{"只有起始标记", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, false},
		{"头部被截断", valid[:6], false},
		{"空数据", nil, false},
		{"PNG 头", []byte{0x89, 0x50, 0x4E, 0x47}, false},
	}
	for _, c := range cases {
		if got := IsJPEG(c.data); got != c.want {
			t.Errorf("%s: IsJPEG = %v, 期望 %v", c.name, got, c.want)
		}
	}
}
