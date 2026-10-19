package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// floodSleepThreshold 单次限流等待的上限，与 Python 版的 FLOOD_SLEEP_THRESHOLD 保持一致。
// 超过该时长不再自动等待，直接把错误抛给用户。
const floodSleepThreshold = 600 * time.Second

// floodMaxRetries 同一个请求最多因限流重试几次。
const floodMaxRetries = 3

// floodWaitMiddleware 在遇到限流类错误时按服务端要求休眠后自动重试。
//
// 等待与重试的骨架用 gotd/contrib 的 SimpleWaiter：它比 tgerr.AsFloodWait 多认
// SLOWMODE_WAIT、2FA_CONFIRM_WAIT、FLOOD_TEST_PHONE_WAIT、FLOOD_SKIP_FAILED_WAIT
// 以及 500 的 WORKER_BUSY_TOO_LONG_RETRY，并按 TDLib 的做法把等待时长夹到 [1s, 14d]。
// gotd 本体不处理这些错误，直接抛出会让单个文件下载中断。
//
// 本文件只保留策略与中文提示：等待上限、重试次数、以及给用户看的进度信息——
// SimpleWaiter 本身是无输出的 invoker，且上限/次数超限时只给英文错误。
type floodWaitMiddleware struct{}

var _ telegram.Middleware = floodWaitMiddleware{}

func (floodWaitMiddleware) Handle(next tg.Invoker) telegram.InvokeFunc {
	// 上限与次数在下面按谓词再设一遍，作为兜底：常见路径由 floodLogInvoker 用中文拦下，
	// 万一出现它不认识的限流错误，SimpleWaiter 也不会无休止地睡下去（最长 14 天）。
	waiter := floodwait.NewSimpleWaiter().
		WithMaxRetries(floodMaxRetries).
		WithMaxWait(floodSleepThreshold)

	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		// 每次请求一个独立的 invoker，重试计数才不会跨请求累加
		return waiter.Handle(&floodLogInvoker{next: next})(ctx, input, output)
	}
}

// floodLogInvoker 夹在 SimpleWaiter 与真正的 invoker 之间，负责两件事：
//   - 打印限流进度（中文）；
//   - 等待时长超过上限、或重试次数用尽时，返回一个**不再被识别为限流**的错误，
//     让 SimpleWaiter 停止重试，并把中文提示交给用户。
//
// 注意这里用 %v 而不是 %w 带上原始错误：一旦用 %w 保留 tgerr 类型，
// SimpleWaiter 会继续当成限流错误接着重试，中文提示就永远到不了用户手上。
type floodLogInvoker struct {
	next     tg.Invoker
	attempts atomic.Int32
}

func (i *floodLogInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	err := i.next.Invoke(ctx, input, output)
	if err == nil {
		return nil
	}

	wait, isFlood := tgerr.AsFloodWait(err)
	if !isFlood {
		return err
	}
	if wait > floodSleepThreshold {
		return fmt.Errorf("触发限流，需等待 %s（超过 %s 的自动等待上限），请稍后再试：%v",
			wait.Round(time.Second), floodSleepThreshold, err)
	}

	attempt := i.attempts.Add(1)
	if int(attempt) > floodMaxRetries {
		return fmt.Errorf("连续 %d 次被限流（每次都已等满服务端要求），仍未能通过，请稍后再试：%v",
			floodMaxRetries, err)
	}

	fmt.Printf("触发限流，等待 %s 后自动重试（第 %d/%d 次）\n",
		wait.Round(time.Second), attempt, floodMaxRetries)
	return err
}
