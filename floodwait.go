package main

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"teledown-go/tools"

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
type floodWaitMiddleware struct {
	mu       sync.Mutex
	cooldown map[reflect.Type]time.Time
}

var _ telegram.Middleware = (*floodWaitMiddleware)(nil)

func (m *floodWaitMiddleware) Handle(next tg.Invoker) telegram.InvokeFunc {
	// 上限与次数在下面按谓词再设一遍，作为兜底：常见路径由 floodLogInvoker 用中文拦下，
	// 万一出现它不认识的限流错误，SimpleWaiter 也不会无休止地睡下去（最长 14 天）。
	waiter := floodwait.NewSimpleWaiter().
		WithMaxRetries(floodMaxRetries).
		WithMaxWait(floodSleepThreshold)

	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		// 每次请求一个独立的 invoker，重试计数才不会跨请求累加
		return waiter.Handle(&floodLogInvoker{next: next, middleware: m})(ctx, input, output)
	}
}

func (m *floodWaitMiddleware) waitForCooldown(ctx context.Context, kind reflect.Type) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		remaining := time.Until(m.cooldown[kind])
		if remaining <= 0 {
			delete(m.cooldown, kind)
		}
		m.mu.Unlock()

		if remaining <= 0 {
			return nil
		}
		if remaining > floodSleepThreshold {
			return fmt.Errorf("同类请求仍在限流，需等待 %s（超过 %s 的自动等待上限），请稍后再试: %w",
				remaining.Round(time.Second), floodSleepThreshold, tools.ErrTransferWaitLimit)
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
			// 等待期间其他在途请求可能延长了冷却时间，发出请求前再检查。
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (m *floodWaitMiddleware) recordCooldown(kind reflect.Type, wait time.Duration) {
	until := time.Now().Add(wait)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cooldown == nil {
		m.cooldown = make(map[reflect.Type]time.Time)
	}
	if until.After(m.cooldown[kind]) {
		m.cooldown[kind] = until
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
	next       tg.Invoker
	middleware *floodWaitMiddleware
	attempts   int
}

// 小文件、缩略图和大文件分片共用上传冷却；不要把发送消息的慢速模式
// 和上传字节混在一起。这个分组是客户端策略，不代表服务端配额的计量范围。
func floodCooldownKind(input bin.Encoder) reflect.Type {
	switch input.(type) {
	case *tg.UploadSaveFilePartRequest, *tg.UploadSaveBigFilePartRequest:
		return reflect.TypeFor[*tg.UploadSaveFilePartRequest]()
	default:
		return reflect.TypeOf(input)
	}
}

func transferWaitClass(input bin.Encoder) tools.TransferWaitClass {
	switch input.(type) {
	case *tg.UploadGetFileRequest, *tg.UploadGetFileHashesRequest, *tg.UploadGetCDNFileRequest,
		*tg.UploadGetCDNFileHashesRequest, *tg.UploadReuploadCDNFileRequest:
		return tools.WaitDownload
	case *tg.UploadSaveFilePartRequest, *tg.UploadSaveBigFilePartRequest:
		return tools.WaitUpload
	case *tg.MessagesSendMediaRequest, *tg.MessagesSendMultiMediaRequest, *tg.MessagesUploadMediaRequest:
		return tools.WaitSubmit
	default:
		return tools.WaitOther
	}
}

func (i *floodLogInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	kind := floodCooldownKind(input)
	if err := i.middleware.waitForCooldown(ctx, kind); err != nil {
		return err
	}
	err := i.next.Invoke(ctx, input, output)
	if err == nil {
		return nil
	}

	rpc, ok := tgerr.As(err)
	if !ok {
		return err
	}
	// Keep this classification in sync with contrib v0.26.0 SimpleWaiter.
	// Only FLOOD_* rate limits share cooldowns across calls; slow mode and
	// other operation-local waits must not stall unrelated traffic.
	seconds := max(1, rpc.Argument)
	switch {
	case rpc.Code == 420 && rpc.IsOneOf(tgerr.FloodWaitErrors...):
		if kind != nil {
			const maxSeconds = int64(1<<63-1) / int64(time.Second)
			i.middleware.recordCooldown(kind, time.Duration(min(int64(seconds), maxSeconds))*time.Second)
		}
	case rpc.Code == 420 && rpc.IsOneOf("SLOWMODE_WAIT", "2FA_CONFIRM_WAIT", "TAKEOUT_INIT_DELAY", "FLOOD_TEST_PHONE_WAIT"):
	case rpc.Code == 420 && rpc.IsType("FLOOD_SKIP_FAILED_WAIT"),
		rpc.Code == 500 && rpc.IsType("WORKER_BUSY_TOO_LONG_RETRY"):
		seconds = 1
	default:
		return err
	}
	// 先比较秒数，避免转换成纳秒时溢出。
	if seconds > int(floodSleepThreshold/time.Second) {
		return fmt.Errorf("触发限流，需等待 %d 秒（超过 %s 的自动等待上限），请稍后再试：%v: %w",
			seconds, floodSleepThreshold, err, tools.ErrTransferWaitLimit)
	}
	wait := time.Duration(seconds) * time.Second

	i.attempts++
	attempt := i.attempts
	if attempt > floodMaxRetries {
		return fmt.Errorf("连续 %d 次被限流（每次都已等满服务端要求），仍未能通过，请稍后再试：%v: %w",
			floodMaxRetries, err, tools.ErrTransferWaitLimit)
	}

	tools.TransferStatsFrom(ctx).RecordFloodWait(transferWaitClass(input), wait)
	detail := ""
	if rpc.IsOneOf(tgerr.ErrPremiumFloodWait) {
		detail = "（Telegram 返回 FLOOD_PREMIUM_WAIT，免费账号的传输速率限制）"
	}
	fmt.Printf("触发限流，等待 %s 后自动重试（第 %d/%d 次）%s\n",
		wait.Round(time.Second), attempt, floodMaxRetries, detail)
	return err
}
