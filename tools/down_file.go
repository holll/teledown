package tools

// 本文件对应 Python 版 tools/down_file.py：下载流程。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// DownloadOptions 是下载命令的参数。
type DownloadOptions struct {
	Range    MsgRange
	Date     DateRange
	Prefix   string
	SavePath string
	Threads  int
	// Resume preserves verified download state for full documents with a known size.
	Resume bool
	Stats  *TransferStats
	// SenderIDs are marked Telegram peer IDs. Nil disables filtering; an empty set matches nobody.
	SenderIDs map[int64]struct{}
	// Manager 用于在 access_hash 失效时清除本地缓存，可为 nil。
	Manager *PeerManager
}

// Download 按范围下载频道内所有可下载媒体。
func Download(ctx context.Context, client *telegram.Client, peer *Peer, opt DownloadOptions) error {
	api := client.API()
	savePath := opt.SavePath
	if savePath == "" {
		savePath = "."
	}
	if err := os.MkdirAll(savePath, 0o755); err != nil {
		return fmt.Errorf("创建保存目录失败: %w", err)
	}
	root, err := os.OpenRoot(savePath)
	if err != nil {
		return fmt.Errorf("打开保存目录失败: %w", err)
	}
	defer root.Close()
	channel := safeDownloadComponent(fmt.Sprintf("%s-%d", peer.Title, peer.ID))
	transfer := newDownloadTransfer(opt.Threads)
	transfer.resume, transfer.stats = opt.Resume, opt.Stats

	var (
		total     int
		succeeded int
		skipped   int
		failed    int
		firstErr  error
	)

	err = EachMessage(ctx, api, peer.Input, opt.Range, opt.Date.Since, func(msg *tg.Message) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 日期过滤：下界在遍历层已提前收工，这里兜住按 ID 拉取与上界两种情况
		if !opt.Date.Contains(msg.Date) || !matchesSender(opt.SenderIDs, msg.FromID) {
			return nil
		}
		info, ok := ExtractMedia(msg)
		if !ok || info.IsSticker {
			return nil
		}

		name := BuildFilename(info.Name, MessageText(msg), info.FileID, info.MIME)
		if !MatchWildcard(opt.Prefix, name) {
			return nil
		}
		total++

		exists, err := saveMedia(ctx, client, root, channel, peer.Input, msg, info, name, transfer)
		if exists {
			skipped++
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 单文件失败不影响其余文件，与 Python 版行为一致
			fmt.Printf("下载出错 %s: %v\n", name, err)
			failed++
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		succeeded++
		return nil
	})
	if err != nil {
		// 缓存的 access_hash 可能已失效：清掉本地缓存，下次运行会重新解析
		if tg.IsChannelInvalid(err) || tg.IsChannelPrivate(err) {
			opt.Manager.ForgetPeer(peer.ID)
			return fmt.Errorf("该频道的访问凭据已失效，已清除本地缓存，请重新运行: %w", err)
		}
		return err
	}

	fmt.Printf("%s 下载完成：成功 %d，跳过（已存在）%d，失败 %d，命中 %d\n",
		peer.Title, succeeded, skipped, failed, total)
	if failed > 0 {
		return fmt.Errorf("下载失败 %d 个文件（成功 %d，跳过 %d）: %w", failed, succeeded, skipped, firstErr)
	}
	return nil
}

func matchesSender(filter map[int64]struct{}, sender tg.PeerClass) bool {
	if filter == nil {
		return true
	}
	var id constant.TDLibPeerID
	switch p := sender.(type) {
	case *tg.PeerUser:
		id.User(p.UserID)
	case *tg.PeerChat:
		id.Chat(p.ChatID)
	case *tg.PeerChannel:
		id.Channel(p.ChannelID)
	default:
		return false
	}
	_, ok := filter[int64(id)]
	return ok
}

func ensureDownloadDir(root *os.Root, path string) error {
	if err := root.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("下载目录不是普通目录: %s", path)
	}
	return nil
}

func saveMedia(ctx context.Context, client *telegram.Client, root *os.Root, channel string, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, name string, transfer downloadTransfer) (bool, error) {
	if err := ensureDownloadDir(root, channel); err != nil {
		return false, err
	}
	exists, target, err := resolveSavePath(root.Lstat, filepath.Join(channel, name), info.Size)
	if err != nil {
		return false, err
	}
	if exists {
		fmt.Printf("媒体已存在：%s\n", filepath.Join(root.Name(), target))
		return true, nil
	}
	return false, downloadOneRoot(ctx, client, peer, msg, info, root, target, transfer)
}

// downloadOne 下载单个媒体，完成后以不覆盖现有文件的方式发布。
func downloadOne(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, target string, threads int) error {
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer root.Close()
	return downloadOneRoot(ctx, client, peer, msg, info, root, filepath.Base(target), newDownloadTransfer(threads))
}

func downloadOneRoot(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, root *os.Root, target string, transfer downloadTransfer) (err error) {
	ref := newDownloadReference(client, peer, msg.ID, info)
	bar := newProgressBar(info.Name, info.Size)
	defer func() { bar.Finish(err == nil) }()
	defer transfer.observeWindow()
	if transfer.resume {
		if identity, ok := newDownloadResumeIdentity(peer, msg, info); ok {
			return downloadWithResume(ctx, client, root, target, info, ref, bar, transfer, identity)
		}
	}

	tmp := filepath.Join(filepath.Dir(target), ".teledown-"+rand.Text()+".downloading")
	file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		_ = file.Close()
		_ = root.Remove(tmp)
	}()
	if err := runDownload(ctx, client, info, ref, file, bar, transfer); err != nil {
		return err
	}
	return publishDownload(ctx, root, file, tmp, target, info.Size)
}

func publishDownload(ctx context.Context, root *os.Root, file *os.File, tmp, target string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size > 0 {
		written, err := file.Stat()
		if err != nil {
			return fmt.Errorf("检查下载大小失败: %w", err)
		}
		if written.Size() != size {
			return fmt.Errorf("下载大小不符：实际 %d，预期 %d", written.Size(), size)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := root.Link(tmp, target); err != nil {
		return fmt.Errorf("保存下载文件失败（不会覆盖现有文件）: %w", err)
	}
	return nil
}

var errDownloadMediaChanged = errors.New("消息刷新后原媒体已不可用或已被替换")

// The SDK coalesces successful refreshes for workers on one builder. Cache
// refresh failures too (gotd leaves the old location unchanged on failure), and
// share the last validated location with a subsequent direct-DC builder.
type downloadReference struct {
	mu       sync.Mutex
	location tg.InputFileLocationClass
	failure  error
	fetch    downloader.RefreshFunc
}

func newDownloadReference(client *telegram.Client, peer tg.InputPeerClass, messageID int, info *mediaInfo) *downloadReference {
	return &downloadReference{location: info.Location, fetch: func(ctx context.Context) (tg.InputFileLocationClass, error) {
		var fresh *tg.Message
		if err := eachMessageByID(ctx, client.API(), peer, []int{messageID}, func(candidate *tg.Message) error {
			if candidate.ID == messageID {
				fresh = candidate
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, errDownloadMediaChanged
		}
		freshInfo, ok := ExtractMedia(fresh)
		if !ok || !sameDownloadMedia(info, freshInfo) {
			return nil, errDownloadMediaChanged
		}
		return freshInfo.Location, nil
	}}
}

func (r *downloadReference) refresh(ctx context.Context) (tg.InputFileLocationClass, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return nil, r.failure
	}
	location, err := r.fetch(ctx)
	if err != nil {
		r.failure = fmt.Errorf("刷新文件引用失败: %w", err)
		return nil, r.failure
	}
	r.location = location
	return location, nil
}

func (r *downloadReference) snapshot() (tg.InputFileLocationClass, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.location, r.failure
}

func sameDownloadMedia(original, fresh *mediaInfo) bool {
	if fresh.IsSticker || original.FileID != fresh.FileID || original.Size != fresh.Size {
		return false
	}
	switch location := original.Location.(type) {
	case *tg.InputDocumentFileLocation:
		other, ok := fresh.Location.(*tg.InputDocumentFileLocation)
		return ok && location.ID == other.ID && location.ThumbSize == other.ThumbSize
	case *tg.InputPhotoFileLocation:
		other, ok := fresh.Location.(*tg.InputPhotoFileLocation)
		return ok && location.ID == other.ID && location.ThumbSize == other.ThumbSize
	default:
		return false
	}
}

type downloadTransfer struct {
	threads   int
	scheduler *downloader.Scheduler
	resume    bool
	stats     *TransferStats
}

// Reuse the adaptive window for every file and retry in a download command.
// Threads sets a ceiling, not the number of requests sent simultaneously at startup.
func newDownloadTransfer(threads int) downloadTransfer {
	threads = max(1, threads)
	return downloadTransfer{
		threads: threads,
		scheduler: downloader.NewScheduler(downloader.SchedulerOptions{
			Min: 1, Start: min(DefaultThreads, threads), Max: threads,
		}),
	}
}

func (d downloadTransfer) observeWindow() {
	if d.stats != nil {
		d.stats.ObserveDownloadWindow(d.scheduler.Snapshot().Window)
	}
}

// gotd does not consistently expose a typed CDN failure. Mark errors at the
// CDN boundary instead of retrying unrelated master or local-write errors.
type downloadCDNError struct{ cause error }

func (e *downloadCDNError) Error() string { return e.cause.Error() }
func (e *downloadCDNError) Unwrap() error { return e.cause }

func tagDownloadCDNError[T any](result T, err error) (T, error) {
	if err != nil {
		err = &downloadCDNError{cause: err}
	}
	return result, err
}

type downloadCDNProvider struct{ downloader.CDNProvider }

func (p downloadCDNProvider) CDN(ctx context.Context, dc int, max int64) (downloader.CDN, io.Closer, error) {
	client, closer, err := p.CDNProvider.CDN(ctx, dc, max)
	if err != nil {
		return nil, closer, &downloadCDNError{cause: err}
	}
	return downloadCDNClient{client}, closer, nil
}

type downloadCDNClient struct{ downloader.CDN }

func (c downloadCDNClient) UploadGetCDNFile(ctx context.Context, req *tg.UploadGetCDNFileRequest) (tg.UploadCDNFileClass, error) {
	return tagDownloadCDNError(c.CDN.UploadGetCDNFile(ctx, req))
}

type downloadCDNMaster struct{ *tg.Client }

func (c downloadCDNMaster) UploadReuploadCDNFile(ctx context.Context, req *tg.UploadReuploadCDNFileRequest) ([]tg.FileHash, error) {
	return tagDownloadCDNError(c.Client.UploadReuploadCDNFile(ctx, req))
}

func (c downloadCDNMaster) UploadGetCDNFileHashes(ctx context.Context, req *tg.UploadGetCDNFileHashesRequest) ([]tg.FileHash, error) {
	return tagDownloadCDNError(c.Client.UploadGetCDNFileHashes(ctx, req))
}

func shouldFallbackDownload(ctx context.Context, err error) bool {
	var cdnErr *downloadCDNError
	if ctx.Err() != nil || !errors.As(err, &cdnErr) || errors.Is(err, downloader.ErrHashMismatch) ||
		errors.Is(err, ErrTransferWaitLimit) || downloader.IsFileReferenceError(err) {
		return false
	}
	_, rateLimited := tgerr.AsFloodWait(err)
	return !rateLimited
}

func (d downloadTransfer) builder(client *telegram.Client, info *mediaInfo, ref *downloadReference, direct bool) *downloader.Builder {
	location, _ := ref.snapshot()
	var builder *downloader.Builder
	if direct {
		builder = client.Downloader().WithAllowCDN(false).Download(client.API(), location)
	} else {
		rpc := downloader.WithCDNProvider(downloadCDNMaster{client.API()}, downloadCDNProvider{client.CDNProvider()})
		builder = client.Downloader().Download(rpc, location)
	}
	// Only full documents have reliable lengths. In particular, the size of a
	// photo variant is not a reliable substitute for the protocol's EOF.
	if loc, ok := info.Location.(*tg.InputDocumentFileLocation); ok && loc.ThumbSize == "" && info.Size > 0 {
		builder.WithSize(info.Size)
	}
	// Master chunk/hash RPCs support refresh, including master calls used by
	// CDN redirect recreation. CDN token RPCs do not carry file_reference;
	// gotd's CDN state machine handles token refresh separately.
	builder.WithThreads(d.threads).WithScheduler(d.scheduler).WithRefresh(ref.refresh)
	if d.stats != nil {
		d.observeWindow()
		builder.WithRetryHandler(func(downloader.RetryEvent) { d.stats.RecordDownloadRetry() })
		builder.WithProgress(func(_, _ int64) { d.observeWindow() })
	}
	return builder
}

func downloadResult(ctx context.Context, ref *downloadReference, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A sibling worker can report a different failure before the refresh
	// error reaches Parallel; a terminal identity mismatch still takes precedence.
	if _, refreshErr := ref.snapshot(); refreshErr != nil {
		return refreshErr
	}
	return err
}

// runDownload 先走带 CDN 能力的下载路径，失败后降级到主 DC 再试一次。
func runDownload(ctx context.Context, client *telegram.Client, info *mediaInfo, ref *downloadReference, file *os.File, bar *progressBar, transfer downloadTransfer) error {
	if err := prepareDownloadFile(ctx, file, info); err != nil {
		return err
	}

	fetchInto := func(builder *downloader.Builder) error {
		w := downloadSink{file: file, bar: bar, size: info.Size}
		_, err := builder.Parallel(ctx, &w)
		if err := downloadResult(ctx, ref, err); err != nil {
			return err
		}
		return w.complete()
	}
	err := fetchInto(transfer.builder(client, info, ref, false))
	if err == nil {
		return file.Sync()
	}
	if !shouldFallbackDownload(ctx, err) {
		return err
	}

	// CDN transport fallback starts over: default master writes in the first
	// pass are unverified and cannot be merged with verified direct writes.
	fmt.Fprintf(os.Stderr, "\nCDN 下载失败，降级到主 DC 重试: %s（%v）\n", info.Name, err)
	if err := prepareDownloadFile(ctx, file, info); err != nil {
		return err
	}
	bar.reset()
	transfer.stats.RecordDownloadRetry()
	legacy := transfer.builder(client, info, ref, true).WithVerify(true)
	if err := fetchInto(legacy); err != nil {
		return fmt.Errorf("CDN 与主 DC 均失败: %w", err)
	}
	return file.Sync()
}

func prepareDownloadFile(ctx context.Context, file *os.File, info *mediaInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("重置临时文件失败: %w", err)
	}
	// Truncate 不会重置顺序写入的偏移；预分配也必须在截断之后执行。
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("重置临时文件偏移失败: %w", err)
	}
	if info.Size > 0 {
		supported, err := preallocateFile(file, info.Size)
		if err != nil {
			return fmt.Errorf("预分配 %s 失败: %w", humanBytes(info.Size), err)
		}
		if !supported {
			fmt.Fprintf(os.Stderr, "当前系统或文件系统不支持预分配磁盘空间，继续下载: %s\n", info.Name)
		}
	}
	return ctx.Err()
}

type byteRange struct{ start, end int64 }

// gotd uses one writer loop even with parallel requests. Keep absolute remote
// offsets, merge adjacent writes and check coverage: file size can hide holes
// and byte totals alone cannot detect overlapping writes.
type downloadSink struct {
	file       *os.File
	bar        *progressBar
	size       int64
	ranges     []byteRange
	checkpoint func(prefix int64) error
}

func (w *downloadSink) prefix() int64 {
	if len(w.ranges) > 0 && w.ranges[0].start == 0 {
		return w.ranges[0].end
	}
	return 0
}

func (w *downloadSink) WriteAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if off < 0 || end < off || (w.size > 0 && end > w.size) {
		return 0, fmt.Errorf("下载分片超出文件范围：%d..%d，预期大小 %d", off, end, w.size)
	}
	if len(p) == 0 {
		return 0, nil
	}
	i := 0
	for i < len(w.ranges) && w.ranges[i].end <= off {
		i++
	}
	if i < len(w.ranges) && end > w.ranges[i].start {
		return 0, fmt.Errorf("下载分片重叠：%d..%d", off, end)
	}
	n, err := w.file.WriteAt(p, off)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	w.bar.Add(int64(n))
	j := i
	if i > 0 && w.ranges[i-1].end == off {
		i--
		off = w.ranges[i].start
	}
	if j < len(w.ranges) && w.ranges[j].start == end {
		end = w.ranges[j].end
		j++
	}
	w.ranges = slices.Replace(w.ranges, i, j, byteRange{off, end})
	if w.checkpoint != nil {
		if err := w.checkpoint(w.prefix()); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (w *downloadSink) complete() error {
	if len(w.ranges) != 1 || w.ranges[0].start != 0 {
		return errors.New("下载内容为空或缺少分片")
	}
	if w.size > 0 && w.ranges[0].end != w.size {
		return fmt.Errorf("下载大小不符：实际 %d，预期 %d", w.ranges[0].end, w.size)
	}
	return nil
}

// DefaultThreads 与 Python 版单线程不同，gotd 支持分片并发，默认开 4 路。
const DefaultThreads = 4
