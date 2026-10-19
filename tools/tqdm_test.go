package tools

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram/uploader"
)

// gotd 的上传器会用多个 goroutine 并发回调 Chunk（回调本身不加锁）。
// 进度只能来自 state.Uploaded 这个累计值，不能靠「读 current 算 delta」，
// 否则并发下会把同一批字节重复计入、进度超过总量。
// 配合 go test -race 还能守住「回调里不得写共享字段」这条。
func TestProgressBarConcurrentChunk(t *testing.T) {
	const total = 1_000_000
	bar := newProgressBar("x.bin", total)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(base int64) {
			defer wg.Done()
			for j := int64(1); j <= 200; j++ {
				_ = bar.Chunk(ctx, uploader.ProgressState{
					Uploaded: base + j*100,
					Total:    total,
				})
			}
		}(int64(i) * 100_000)
	}
	wg.Wait()

	if got := bar.current.Load(); got < 0 || got > total {
		t.Errorf("并发回调后进度 = %d，越界（应在 0..%d）", got, total)
	}
}

// 顺序回调（单线程下载路径）下进度必须精确等于最后一次的值。
func TestProgressBarSequentialChunk(t *testing.T) {
	bar := newProgressBar("x.bin", 1000)
	for _, uploaded := range []int64{100, 300, 1000} {
		_ = bar.Chunk(context.Background(), uploader.ProgressState{
			Uploaded: uploaded, Total: 1000,
		})
	}
	if got := bar.current.Load(); got != 1000 {
		t.Errorf("进度 = %d，期望 1000", got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1:00"},
		{65 * time.Second, "1:05"},
		{10 * time.Minute, "10:00"},
		{time.Hour, "1:00:00"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
		{-time.Second, "0s"}, // 负值兜底成 0
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 剩余时间用整体平均速度估算；样本不足或已下完时不显示（返回 ok=false）。
func TestEtaFor(t *testing.T) {
	const mb = 1 << 20
	if eta, ok := etaFor(5*mb, mb, time.Second); !ok || eta != 4*time.Second {
		t.Errorf("已下 1MB/5MB 用时 1s 应剩 4s，得到 %v ok=%v", eta, ok)
	}
	for _, c := range []struct {
		name  string
		total int64
		got   int64
		d     time.Duration
	}{
		{"样本不足 1s 不显示", 5 * mb, mb, 500 * time.Millisecond},
		{"已下载完不显示", 5 * mb, 5 * mb, 10 * time.Second},
		{"文件大小未知不显示", 0, mb, 10 * time.Second},
		{"还没开始下载不显示", 5 * mb, 0, 10 * time.Second},
	} {
		if _, ok := etaFor(c.total, c.got, c.d); ok {
			t.Errorf("%s：etaFor(%d, %d, %v) 不应给出剩余时间", c.name, c.total, c.got, c.d)
		}
	}
}

// 进行中的一行要同时带出百分比、字节数、速度、已用与剩余时间。
func TestProgressLineFields(t *testing.T) {
	bar := newProgressBar("a.mp4", 8<<20)
	line := bar.progressLine(2<<20, 4*time.Second)
	// 2MB/8MB、用时 4s、512KB/s → 剩余 12s
	for _, want := range []string{"25.0%", "2.00MB/8.00MB", "512.00KB/s", "已用 4s", "剩余 12s"} {
		if !strings.Contains(line, want) {
			t.Errorf("进度行 %q 缺少 %q", line, want)
		}
	}
}

// 文件大小未知（流式上传）时不显示百分比与剩余时间，也不能写成 "1.00MB/0B"。
func TestProgressLineWithoutTotal(t *testing.T) {
	bar := newProgressBar("a.bin", 0)
	line := bar.progressLine(1<<20, 3*time.Second)
	if strings.Contains(line, "%") || strings.Contains(line, "剩余") || strings.Contains(line, "0B") {
		t.Errorf("大小未知时只应报已传字节：%q", line)
	}
	if !strings.Contains(line, "已用 3s") {
		t.Errorf("大小未知时仍应显示已用时间：%q", line)
	}
}

// 终端窄的时候缩的是文件名，时间字段不能被挤掉。
func TestProgressLineNarrowTerminal(t *testing.T) {
	bar := newProgressBar("一个非常非常非常长的中文文件名用来测试截断.mp4", 10<<20)
	line := bar.progressLineWidth(3<<20, 9*time.Second, 60)
	if n := len([]rune(line)); n > 60 {
		t.Errorf("行宽 %d 超过终端宽度 60：%q", n, line)
	}
	for _, want := range []string{"30.0%", "已用 9s", "剩余 21s"} {
		if !strings.Contains(line, want) {
			t.Errorf("窄终端下不该丢掉 %q：%q", want, line)
		}
	}
	if strings.Contains(line, "用来测试截断.mp4") {
		t.Errorf("文件名应被截断：%q", line)
	}
}

// 完成后的条目要留在终端里（这是这批改动的重点），而不是被清掉。
func TestProgressBarFinishKeepsSummary(t *testing.T) {
	out := captureStderr(t, func() {
		bar := newProgressBar("第01集.mp4", 8<<20)
		bar.now = func() time.Time { return bar.start.Add(62 * time.Second) }
		bar.current.Store(8 << 20)
		bar.Finish(true)
	})

	for _, want := range []string{"完成", "第01集.mp4", "8.00MB", "用时 1:02", "平均"} {
		if !strings.Contains(out, want) {
			t.Errorf("汇总行 %q 缺少 %q", out, want)
		}
	}
	// 输出被重定向（非终端）时不应留下 \r 之类的控制字符
	if strings.ContainsAny(out, "\r") {
		t.Errorf("非终端环境下不应输出控制字符：%q", out)
	}
}

// 失败时只清掉进行中的一行，具体错误交给调用方打印，避免两行混在一起。
func TestProgressBarFinishFailureStaysQuiet(t *testing.T) {
	out := captureStderr(t, func() {
		bar := newProgressBar("坏文件.mp4", 100)
		bar.current.Store(10)
		bar.Finish(false)
	})
	if strings.Contains(out, "完成") || strings.Contains(out, "坏文件.mp4") {
		t.Errorf("失败时不应输出汇总行：%q", out)
	}
}

// captureStderr 把 stderr 换成管道，返回函数执行期间写入的内容。
// 注意要在回调里创建 progressBar：newProgressBar 会读 stderr 判断是不是终端。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
