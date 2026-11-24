package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// ErrTransferWaitLimit marks an exhausted or disallowed wait policy. It does
// not wrap the raw RPC flood error: SDK retry loops must stop at this boundary.
var ErrTransferWaitLimit = errors.New("达到自动限流等待上限")

// TransferStats holds command-scoped aggregate counts. It contains no filenames,
// peer identifiers or request payloads and is safe for partition workers to share.
type TransferStats struct {
	window, peak                   atomic.Int64
	downloadRetries, uploadRetries atomic.Int64
	mediaSendFailures              atomic.Int64
	waits                          [waitClassCount]waitStats
}

type TransferWaitClass uint8

const (
	WaitDownload TransferWaitClass = iota
	WaitUpload
	WaitSubmit
	WaitOther
	waitClassCount
)

type waitStats struct {
	events atomic.Int64
	time   atomic.Int64
}

type transferStatsKey struct{}

func WithTransferStats(ctx context.Context, stats *TransferStats) context.Context {
	if stats == nil {
		return ctx
	}
	return context.WithValue(ctx, transferStatsKey{}, stats)
}

func TransferStatsFrom(ctx context.Context) *TransferStats {
	stats, _ := ctx.Value(transferStatsKey{}).(*TransferStats)
	return stats
}

func (s *TransferStats) ObserveDownloadWindow(window int) {
	if s == nil || window < 1 {
		return
	}
	v := int64(window)
	s.window.Store(v)
	for old := s.peak.Load(); old < v && !s.peak.CompareAndSwap(old, v); old = s.peak.Load() {
	}
}

func (s *TransferStats) RecordDownloadRetry() {
	if s != nil {
		s.downloadRetries.Add(1)
	}
}

func (s *TransferStats) RecordUploadRetry() {
	if s != nil {
		s.uploadRetries.Add(1)
	}
}

func (s *TransferStats) RecordMediaSendFailure() {
	if s != nil {
		s.mediaSendFailures.Add(1)
	}
}

// RecordFloodWait records a server-advertised delay accepted for retry, not time
// actually slept. Concurrent waits can overlap or be cancelled.
func (s *TransferStats) RecordFloodWait(class TransferWaitClass, delay time.Duration) {
	if s != nil && class < waitClassCount && delay > 0 {
		s.waits[class].events.Add(1)
		s.waits[class].time.Add(int64(delay))
	}
}

func (s *TransferStats) Report(w io.Writer) {
	if s == nil {
		return
	}
	fmt.Fprintln(w, "传输诊断（等待为接受的服务端要求之和，不等于实际耗时）：")
	if window := s.window.Load(); window > 0 {
		fmt.Fprintf(w, "  下载并发窗口（采样）：最后 %d，峰值 %d\n", window, s.peak.Load())
	}
	fmt.Fprintf(w, "  重试：下载 %d，上传同分片 %d；媒体提交失败 %d\n", s.downloadRetries.Load(), s.uploadRetries.Load(), s.mediaSendFailures.Load())
	for i, name := range [...]string{"下载字节", "上传字节", "消息提交", "其他请求"} {
		wait := &s.waits[i]
		fmt.Fprintf(w, "  %s限流：%d 次，%s\n", name, wait.events.Load(), time.Duration(wait.time.Load()))
	}
}
