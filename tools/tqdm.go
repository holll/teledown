package tools

// 本文件对应 Python 版 tools/tqdm.py：进度显示。
//
// 输出分两层：
//   - 进行中：一行原地刷新，显示百分比、字节数、速度、已用时间与剩余时间（ETA）；
//   - 完成后：Finish 把这一行换成一条汇总行并换行，**保留在终端里**，
//     这样一批文件跑完后还能回看每个文件下过、用时多久、平均多快。
//
// stderr 不是终端时（重定向到文件/管道）不画进行中的行，只留汇总行，
// 免得日志里塞满 \r 与清行转义。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"golang.org/x/term"
)

// progressInterval 是进度行的刷新节流间隔：太频繁既刷屏又拖慢传输。
const progressInterval = 200 * time.Millisecond

// etaMinElapsed 是显示剩余时间所需的最短已用时长。
// 刚起步时样本太少，拿几十毫秒的速率外推会得到离谱的数字，不如先不显示。
const etaMinElapsed = time.Second

// progressNameMax / progressNameMin 是文件名在进度行里占的宽度上限与下限。
const (
	progressNameMax = 40
	progressNameMin = 6
)

// progressBar 是下载与上传共用的进度显示。
type progressBar struct {
	name    string
	total   int64
	current atomic.Int64
	start   time.Time
	last    atomic.Int64 // 上次打印时间（UnixNano）

	// now 便于测试注入固定时钟；interactive 表示 stderr 是不是终端。
	now         func() time.Time
	interactive bool
}

func newProgressBar(name string, total int64) *progressBar {
	return &progressBar{
		name:        name,
		total:       total,
		start:       time.Now(),
		now:         time.Now,
		interactive: term.IsTerminal(int(os.Stderr.Fd())),
	}
}

// Add 累加已完成的字节数，由下载写入器调用。
func (p *progressBar) Add(n int64) {
	p.report(p.current.Add(n))
}

// Chunk 实现 uploader.Progress，供上传器回调。
//
// gotd 的 bigLoop 会用 u.threads 个 goroutine 并发回调（回调本身没有加锁），所以：
//   - state.Uploaded 已是累计值，直接覆盖即可；用「读 current 再算 delta」的写法在并发下
//     会让多个回调读到同一个旧值，把同一批字节重复计入，百分比能超过 100%；
//   - 不在这里写 p.total：构造函数已按文件大小设好，再写就与 report 的读构成数据竞争。
func (p *progressBar) Chunk(_ context.Context, state uploader.ProgressState) error {
	p.current.Store(state.Uploaded)
	p.report(state.Uploaded)
	return nil
}

// reset 在重新从头下载之前清零已下载字节数。
// 刻意不动 start：汇总行里的「用时」应当是这个文件真实花掉的时间（含失败的那一轮）。
func (p *progressBar) reset() {
	p.current.Store(0)
}

// Finish 结束当前文件的进度显示。
//
// ok 为真时留一条汇总行（已完成的条目会一直留在终端里）；失败时只清掉进行中的一行，
// 具体错误由调用方打印，避免把两行内容混在一起。
func (p *progressBar) Finish(ok bool) {
	if p.interactive {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if !ok {
		return
	}

	got := p.current.Load()
	if got == 0 && p.total > 0 {
		// 文件太小、一次回调都没触发时兜底，避免汇总行显示 0B
		got = p.total
	}
	fmt.Fprintln(os.Stderr, p.summaryLine(got, p.elapsed()))
}

// elapsed 返回从这个文件开始下载至今的时长。
func (p *progressBar) elapsed() time.Duration {
	return p.now().Sub(p.start)
}

func (p *progressBar) report(got int64) {
	if !p.interactive {
		return
	}

	now := p.now()
	last := p.last.Load()
	if now.UnixNano()-last < int64(progressInterval) {
		return
	}
	// 只有一个 goroutine 能抢到这次打印
	if !p.last.CompareAndSwap(last, now.UnixNano()) {
		return
	}

	fmt.Fprintf(os.Stderr, "\r%s\033[K", p.progressLine(got, now.Sub(p.start)))
}

// progressLine 组装进行中的那一行（宽度取当前终端）。
func (p *progressBar) progressLine(got int64, elapsed time.Duration) string {
	return p.progressLineWidth(got, elapsed, terminalWidth())
}

// progressLineWidth 按给定终端宽度组装进度行。
//
// 例：  第01集 上.mp4  50.0%  328.12MB/656.25MB  12.62MB/s  已用 26s  剩余 26s
//
// 宽度不够时缩的是文件名：时间等字段比文件名更需要完整显示。
func (p *progressBar) progressLineWidth(got int64, elapsed time.Duration, width int) string {
	fields := make([]string, 0, 5)
	if p.total > 0 {
		fields = append(fields, fmt.Sprintf("%.1f%%", float64(got)*100/float64(p.total)))
		fields = append(fields, fmt.Sprintf("%s/%s", humanBytes(got), humanBytes(p.total)))
	} else {
		// 大小未知（如流式上传）时只报已传字节，写 "1.00MB/0B" 会误导
		fields = append(fields, humanBytes(got))
	}
	fields = append(fields, humanBytes(rate(got, elapsed))+"/s")
	fields = append(fields, "已用 "+formatDuration(elapsed))
	if eta, ok := etaFor(p.total, got, elapsed); ok {
		fields = append(fields, "剩余 "+formatDuration(eta))
	}

	rest := strings.Join(fields, "  ")
	nameWidth := progressNameMax
	if width > 0 {
		if left := width - len([]rune(rest)) - 4; left < nameWidth {
			nameWidth = left
		}
	}
	// 文件名至少留几个字符；终端窄到连字段都放不下时就让它溢出，
	// 总比把「剩余时间」这类信息砍掉更有用。
	if nameWidth < progressNameMin {
		nameWidth = progressNameMin
	}
	return "  " + shorten3(p.name, nameWidth) + "  " + rest
}

// summaryLine 组装完成后的汇总行，例如：
//
//	完成  第01集.mp4  8.00MB  用时 1:02  平均 132.15KB/s
func (p *progressBar) summaryLine(got int64, elapsed time.Duration) string {
	fields := []string{
		"完成",
		shorten3(p.name, progressNameMax),
		humanBytes(got),
		"用时 " + formatDuration(elapsed),
	}
	if speed := rate(got, elapsed); speed > 0 {
		fields = append(fields, "平均 "+humanBytes(speed)+"/s")
	}
	return "  " + strings.Join(fields, "  ")
}

// rate 返回整体平均速度（字节/秒）；时长不足时返回 0。
func rate(got int64, elapsed time.Duration) int64 {
	seconds := elapsed.Seconds()
	if got <= 0 || seconds <= 0 {
		return 0
	}
	speed := float64(got) / seconds
	if speed <= 0 {
		return 0
	}
	return int64(speed)
}

// etaFor 用整体平均速度估算剩余时间。
// 文件大小未知、已下载完、或已用时长不足（样本太少会算出离谱的值）时返回 ok=false。
func etaFor(total, got int64, elapsed time.Duration) (time.Duration, bool) {
	if total <= 0 || got <= 0 || got >= total || elapsed < etaMinElapsed {
		return 0, false
	}
	speed := float64(rate(got, elapsed))
	if speed <= 0 {
		return 0, false
	}
	remain := float64(total-got) / speed
	if remain <= 0 {
		return 0, false
	}
	return time.Duration(remain * float64(time.Second)), true
}

// formatDuration 把时长格式化成 12s / 3:05 / 1:02:03 三种粒度。
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int64(d.Round(time.Second).Seconds())
	switch h, m, s := seconds/3600, (seconds%3600)/60, seconds%60; {
	case h > 0:
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	case m > 0:
		return fmt.Sprintf("%d:%02d", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// terminalWidth 取终端宽度，取不到时返回 0（调用方按默认宽度处理）。
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f%s", value, u)
		}
	}
	return fmt.Sprintf("%.2fPB", value/unit)
}

func shorten3(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 3 {
		return string(r[:limit])
	}
	return string(r[:limit-3]) + "..."
}

var _ uploader.Progress = (*progressBar)(nil)
