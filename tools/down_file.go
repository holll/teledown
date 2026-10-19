package tools

// 本文件对应 Python 版 tools/down_file.py：下载流程。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

// DownloadOptions 是下载命令的参数。
type DownloadOptions struct {
	Range    MsgRange
	Date     DateRange
	Prefix   string
	SavePath string
	Threads  int
	// Manager 用于在 access_hash 失效时清除本地缓存，可为 nil。
	Manager *PeerManager
}

// Download 按范围下载频道内所有可下载媒体。
func Download(ctx context.Context, client *telegram.Client, peer *Peer, opt DownloadOptions) error {
	api := client.API()
	// 频道根目录：save_path/标题-ID，其下再按消息月份分子目录（见 MonthDir）
	dir := filepath.Join(opt.SavePath, fmt.Sprintf("%s-%d", peer.Title, peer.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建保存目录失败: %w", err)
	}

	var (
		total     int
		succeeded int
		skipped   int
		failed    int
		firstErr  error
	)

	err := EachMessage(ctx, api, peer.Input, opt.Range, opt.Date.Since, func(msg *tg.Message) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 日期过滤：下界在遍历层已提前收工，这里兜住按 ID 拉取与上界两种情况
		if !opt.Date.Contains(msg.Date) {
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

		// 按消息发送月份归档到子目录，同一月份重复创建是幂等的
		monthDir := filepath.Join(dir, MonthDir(msg.Date))
		if err := os.MkdirAll(monthDir, 0o755); err != nil {
			return fmt.Errorf("创建月份目录失败: %w", err)
		}

		exists, target := ResolveSavePath(filepath.Join(monthDir, name), info.Size)
		if exists {
			fmt.Printf("媒体已存在：%s\n", target)
			skipped++
			return nil
		}

		if err := downloadOne(ctx, client, peer.Input, msg, info, target, opt.Threads); err != nil {
			// 单文件失败不影响其余文件，与 Python 版行为一致
			fmt.Printf("下载出错 %s: %v\n", name, err)
			failed++
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				return ctx.Err()
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

// downloadOne 下载单个媒体：写 .downloading 临时文件，成功后改名。
// file_reference 过期时重新拉取消息刷新引用并重试一次。
//
// gotd 的 downloader 不会自愈过期的 file_reference（只有 CDN 的 token/密钥刷新），
// 所以这一步重试是文档下载失败后唯一的恢复手段。
func downloadOne(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, target string, threads int) (err error) {
	file, err := os.CreateTemp(filepath.Dir(target), ".teledown-*.downloading")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmp := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}()
	// 进度条按「一个文件一条」的生命周期：重试共用同一条，用时也只算一次，
	// 结束后 Finish 把这一行换成汇总行留在终端里。
	bar := newProgressBar(info.Name, info.Size)
	defer func() { bar.Finish(err == nil) }()

	const attempts = 2
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		runErr := runDownload(ctx, client, info.Location, file, info.Name, bar, threads)
		if runErr == nil {
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("关闭临时文件失败: %w", closeErr)
			}
			if renameErr := os.Rename(tmp, target); renameErr != nil {
				return fmt.Errorf("重命名临时文件失败: %w", renameErr)
			}
			return nil
		}
		lastErr = runErr

		if !tg.IsFileReferenceExpired(runErr) {
			return runErr
		}
		if attempt == attempts-1 {
			break
		}
		// 换行打印，免得被进行中的进度行覆盖掉
		fmt.Fprintf(os.Stderr, "\n文件引用已过期，重新获取消息后重试: %s\n", info.Name)

		fresh, refetchErr := refetchMessage(ctx, client.API(), peer, msg.ID)
		if refetchErr != nil {
			return refetchErr
		}
		freshInfo, ok := ExtractMedia(fresh)
		if !ok {
			return fmt.Errorf("消息刷新后已无可下载媒体")
		}
		info = freshInfo
	}
	return fmt.Errorf("文件引用过期且重试无效: %w", lastErr)
}

// runDownload 先走带 CDN 能力的下载路径，失败后降级到主 DC 再试一次。
//
// 必须用 client.Download() 而不是 downloader.NewDownloader().Download(api, loc)：
// 后者的 allowCDN 是 nil 且 *tg.Client 不实现 downloader.CDNProvider，
// 会被 builder 兜底回退到 legacy 路径，等于让 Options.AllowCDN 失效。
func runDownload(ctx context.Context, client *telegram.Client, loc tg.InputFileLocationClass, file *os.File, name string, bar *progressBar, threads int) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("重置临时文件失败: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("重置临时文件偏移失败: %w", err)
	}
	bar.reset()

	err := fetch(ctx, client.Download(loc), file, bar, threads)
	if err == nil {
		return file.Sync()
	}

	// CDN 连接建立失败或 CDN 节点不可达时不会自动回退，这里降级到主 DC 重试一次。
	// 降级路径会带 cdn_supported=false 重新请求，服务端改从主 DC 提供数据。
	if ctx.Err() != nil {
		return err
	}
	firstErr := err
	// 换行打印，免得被进行中的进度行覆盖
	fmt.Fprintf(os.Stderr, "\nCDN 下载失败，降级到主 DC 重试: %s（%v）\n", name, firstErr)

	if truncErr := file.Truncate(0); truncErr != nil {
		return fmt.Errorf("重置临时文件失败: %w", truncErr)
	}
	// Truncate 只改文件长度、不改写偏移：单线程走的是顺序写（Stream），
	// 不回到开头会把降级后的数据接在上一轮的偏移后面，产出带空洞的坏文件。
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return fmt.Errorf("重置临时文件偏移失败: %w", seekErr)
	}
	bar.reset()
	// 显式关掉 CDN：这条路径就是要走主 DC（不带 cdn_supported 重新请求），
	// 不能依赖「NewDownloader 默认 allowCDN 为 nil」这个实现细节。
	legacy := downloader.NewDownloader().WithAllowCDN(false).Download(client.API(), loc).
		WithVerify(true)
	if err := fetch(ctx, legacy, file, bar, threads); err != nil {
		return fmt.Errorf("CDN 与主 DC 均失败: %w", err)
	}
	return file.Sync()
}

// fetch 按并发数选择 Parallel（WriterAt）或 Stream（Writer）执行一次下载。
func fetch(ctx context.Context, builder *downloader.Builder, file *os.File, bar *progressBar, threads int) error {
	builder = builder.WithThreads(threads)
	if threads > 1 {
		// 多线程下载需要 WriterAt，进度通过包装写入器统计
		_, err := builder.Parallel(ctx, &progressWriterAt{file: file, bar: bar})
		return err
	}
	_, err := builder.Stream(ctx, &writerProgress{file: file, bar: bar})
	return err
}

// progressWriterAt 多线程下载时按偏移写盘，顺带统计进度。
type progressWriterAt struct {
	file *os.File
	bar  *progressBar
}

func (w *progressWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := w.file.WriteAt(p, off)
	w.bar.Add(int64(n))
	return n, err
}

// writerProgress 单线程下载时统计进度。
type writerProgress struct {
	file *os.File
	bar  *progressBar
}

func (w *writerProgress) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	w.bar.Add(int64(n))
	return n, err
}

// DefaultThreads 与 Python 版单线程不同，gotd 支持分片并发，默认开 4 路。
const DefaultThreads = 4
