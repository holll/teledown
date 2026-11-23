package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// scriptedInvoker 按预设脚本返回错误：脚本用完后一直返回最后一个。
type scriptedInvoker struct {
	errs  []error
	calls int
}

func (s *scriptedInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	if len(s.errs) > 1 {
		s.errs = s.errs[1:]
	}
	return err
}

// floodWait 构造服务端那种 FLOOD_WAIT 错误（tgerr.New 会从 FLOOD_WAIT_20 里解出参数）。
func floodWait(seconds int) error {
	return tgerr.New(420, fmt.Sprintf("FLOOD_WAIT_%d", seconds))
}

// 可接受的等待时长：原样把限流错误交回给 SimpleWaiter（它负责真的等待并重试），
// 同时打出一条中文进度提示。
func TestFloodLogInvokerPassesRetryableWait(t *testing.T) {
	inv := &floodLogInvoker{middleware: &floodWaitMiddleware{}, next: &scriptedInvoker{errs: []error{floodWait(20)}}}

	out := captureStdout(t, func() {
		err := inv.Invoke(context.Background(), nil, nil)
		if _, isFlood := tgerr.AsFloodWait(err); !isFlood {
			t.Errorf("可等待的限流错误应原样返回，让 SimpleWaiter 继续处理，得到 %v", err)
		}
	})

	if !strings.Contains(out, "触发限流，等待 20s") || !strings.Contains(out, "第 1/3 次") {
		t.Errorf("缺少限流提示或次数，实际输出 %q", out)
	}
}

// 等待时长超过上限：必须返回一个**不再被识别为限流**的错误，否则 SimpleWaiter
// 会一直等下去（上限形同虚设）。
func TestFloodLogInvokerStopsOnTooLongWait(t *testing.T) {
	inv := &floodLogInvoker{middleware: &floodWaitMiddleware{}, next: &scriptedInvoker{errs: []error{floodWait(int(floodSleepThreshold/time.Second) + 1)}}}

	err := inv.Invoke(context.Background(), nil, nil)
	if _, isFlood := tgerr.AsFloodWait(err); isFlood {
		t.Fatal("超长等待必须返回非限流错误，否则会被无限重试")
	}
	if !strings.Contains(err.Error(), "自动等待上限") {
		t.Errorf("错误提示不明确: %v", err)
	}
}

// 重试次数用尽后同样要变成非限流错误，让调用方拿到中文提示而不是继续重试。
func TestFloodLogInvokerStopsAfterMaxRetries(t *testing.T) {
	inv := &floodLogInvoker{middleware: &floodWaitMiddleware{}, next: &scriptedInvoker{errs: []error{floodWait(1)}}}

	for attempt := 1; attempt <= floodMaxRetries; attempt++ {
		err := inv.Invoke(context.Background(), nil, nil)
		if _, isFlood := tgerr.AsFloodWait(err); !isFlood {
			t.Fatalf("第 %d 次仍应在重试额度内，得到 %v", attempt, err)
		}
	}

	err := inv.Invoke(context.Background(), nil, nil)
	if _, isFlood := tgerr.AsFloodWait(err); isFlood {
		t.Fatal("超过重试次数后必须停止重试")
	}
	if !strings.Contains(err.Error(), "连续 3 次被限流") {
		t.Errorf("错误提示不明确: %v", err)
	}
}

// 非限流错误不能被吞掉或改造。
func TestFloodLogInvokerPassesOtherErrors(t *testing.T) {
	want := errors.New("boom")
	inv := &floodLogInvoker{middleware: &floodWaitMiddleware{}, next: &scriptedInvoker{errs: []error{want}}}

	if got := inv.Invoke(context.Background(), nil, nil); !errors.Is(got, want) {
		t.Errorf("非限流错误应原样返回，得到 %v", got)
	}
}

// captureStdout 把 stdout 换成管道，返回回调执行期间写入的内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdout = w
	defer func() { os.Stdout = orig }()

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

func TestFloodWaitMiddlewareSharesCooldownByRPCType(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &floodWaitMiddleware{}
		ctx := t.Context()
		start := time.Now()
		first := &scriptedInvoker{errs: []error{floodWait(5), nil}}
		done := make(chan error, 1)
		go func() { done <- m.Handle(first)(ctx, &tg.UploadGetFileRequest{}, nil) }()
		synctest.Wait()
		if first.calls != 1 {
			t.Fatalf("首次分片应已触发限流，实际调用 %d 次", first.calls)
		}

		next := &scriptedInvoker{}
		invoke := m.Handle(next)
		if err := invoke(ctx, &tg.HelpGetConfigRequest{}, nil); err != nil || next.calls != 1 || time.Since(start) != 0 {
			t.Fatalf("不同类型的请求不应等待：err=%v, calls=%d, elapsed=%s", err, next.calls, time.Since(start))
		}

		shortCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := invoke(shortCtx, &tg.UploadGetFileRequest{}, nil); !errors.Is(err, context.DeadlineExceeded) || next.calls != 1 {
			t.Fatalf("等待中的请求应该能取消且不发送：err=%v, calls=%d", err, next.calls)
		}
		if err := invoke(ctx, &tg.UploadGetFileRequest{}, nil); err != nil || next.calls != 2 || time.Since(start) != 5*time.Second {
			t.Fatalf("同类请求应等到共享冷却结束：err=%v, calls=%d, elapsed=%s", err, next.calls, time.Since(start))
		}
		if err := <-done; err != nil || first.calls != 2 {
			t.Fatalf("原请求应独立重试成功：err=%v, calls=%d", err, first.calls)
		}
	})
}

func TestFloodWaitMiddlewareSharesCooldownOverLimit(t *testing.T) {
	m := &floodWaitMiddleware{}
	next := &scriptedInvoker{errs: []error{floodWait(900)}}
	invoke := m.Handle(next)
	for _, want := range []string{"触发限流", "同类请求仍在限流"} {
		err := invoke(t.Context(), &tg.UploadGetFileRequest{}, nil)
		if err == nil || !strings.Contains(err.Error(), want) || next.calls != 1 {
			t.Fatalf("err=%v, calls=%d; 应包含 %q 且只发送一次", err, next.calls, want)
		}
	}
}

func TestFloodWaitMiddlewareDoesNotInvokeCanceledRequest(t *testing.T) {
	next := &scriptedInvoker{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	invoke := (&floodWaitMiddleware{}).Handle(next)
	if err := invoke(ctx, nil, nil); !errors.Is(err, context.Canceled) || next.calls != 0 {
		t.Fatalf("err = %v, calls = %d; want canceled with no calls", err, next.calls)
	}
}
