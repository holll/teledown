package tools

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransferStatsConcurrentReport(t *testing.T) {
	stats := new(TransferStats)
	ctx := WithTransferStats(t.Context(), stats)
	var wg sync.WaitGroup
	for window := 1; window <= 8; window++ {
		wg.Go(func() {
			s := TransferStatsFrom(ctx)
			s.ObserveDownloadWindow(window)
			s.RecordDownloadRetry()
			s.RecordUploadRetry()
			s.RecordFloodWait(WaitUpload, time.Second)
		})
	}
	wg.Wait()
	stats.RecordMediaSendFailure()
	var out bytes.Buffer
	stats.Report(&out)
	for _, want := range []string{"峰值 8", "下载 8，上传同分片 8；媒体提交失败 1", "上传字节限流：8 次，8s", "消息提交限流：0 次"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q: %s", want, out.String())
		}
	}
}

func TestTransferStatsDisabled(t *testing.T) {
	ctx := t.Context()
	if WithTransferStats(ctx, nil) != ctx {
		t.Fatal("disabled diagnostics should reuse the original context")
	}
	stats := TransferStatsFrom(ctx)
	stats.ObserveDownloadWindow(4)
	stats.RecordDownloadRetry()
	stats.RecordUploadRetry()
	stats.RecordMediaSendFailure()
	stats.RecordFloodWait(WaitSubmit, time.Second)
	var out bytes.Buffer
	stats.Report(&out)
	if out.Len() != 0 {
		t.Fatal("disabled diagnostics wrote output")
	}
}
