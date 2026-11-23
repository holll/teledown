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

	"github.com/gotd/td/constant"
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

		exists, err := saveMedia(ctx, client, root, channel, peer.Input, msg, info, name, opt.Threads)
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

func saveMedia(ctx context.Context, client *telegram.Client, root *os.Root, channel string, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, name string, threads int) (bool, error) {
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
	return false, downloadOneRoot(ctx, client, peer, msg, info, root, target, threads)
}

// downloadOne 下载单个媒体：写 .downloading 临时文件，成功后改名。
// file_reference 过期时重新拉取消息刷新引用并重试一次。
//
// gotd 的 downloader 不会自愈过期的 file_reference（只有 CDN 的 token/密钥刷新），
// 所以这一步重试是文档下载失败后唯一的恢复手段。
func downloadOne(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, target string, threads int) error {
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer root.Close()
	return downloadOneRoot(ctx, client, peer, msg, info, root, filepath.Base(target), threads)
}

func downloadOneRoot(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo, root *os.Root, target string, threads int) (err error) {
	tmp := filepath.Join(filepath.Dir(target), ".teledown-"+rand.Text()+".downloading")
	file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		_ = file.Close()
		_ = root.Remove(tmp)
	}()
	// 进度条按「一个文件一条」的生命周期：重试共用同一条，用时也只算一次，
	// 结束后 Finish 把这一行换成汇总行留在终端里。
	bar := newProgressBar(info.Name, info.Size)
	defer func() { bar.Finish(err == nil) }()

	const attempts = 2
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		runErr := runDownload(ctx, client, info, file, bar, threads)
		if runErr == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if info.Size > 0 {
				written, statErr := file.Stat()
				if statErr != nil {
					return fmt.Errorf("检查下载大小失败: %w", statErr)
				}
				if written.Size() != info.Size {
					return fmt.Errorf("下载大小不符：实际 %d，预期 %d", written.Size(), info.Size)
				}
			}
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("关闭临时文件失败: %w", closeErr)
			}
			if linkErr := root.Link(tmp, target); linkErr != nil {
				return fmt.Errorf("保存下载文件失败（不会覆盖现有文件）: %w", linkErr)
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

		var fresh *tg.Message
		refetchErr := eachMessageByID(ctx, client.API(), peer, []int{msg.ID}, func(candidate *tg.Message) error {
			if candidate.ID == msg.ID {
				fresh = candidate
			}
			return nil
		})
		if refetchErr != nil {
			return refetchErr
		}
		if fresh == nil {
			return fmt.Errorf("原消息不可用，无法刷新文件引用")
		}
		freshInfo, ok := ExtractMedia(fresh)
		if !ok || freshInfo.FileID != info.FileID || freshInfo.IsSticker {
			return fmt.Errorf("消息刷新后原媒体已不可用")
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
func runDownload(ctx context.Context, client *telegram.Client, info *mediaInfo, file *os.File, bar *progressBar, threads int) error {
	if err := prepareDownloadFile(ctx, file, info); err != nil {
		return err
	}
	bar.reset()

	err := fetch(ctx, client.Download(info.Location), file, bar, threads)
	if err == nil {
		return file.Sync()
	}

	// CDN 连接建立失败或 CDN 节点不可达时不会自动回退，这里降级到主 DC 重试一次。
	// 降级路径会带 cdn_supported=false 重新请求，服务端改从主 DC 提供数据。
	if ctx.Err() != nil {
		return err
	}
	// 换行打印，免得被进行中的进度行覆盖
	fmt.Fprintf(os.Stderr, "\nCDN 下载失败，降级到主 DC 重试: %s（%v）\n", info.Name, err)

	if err := prepareDownloadFile(ctx, file, info); err != nil {
		return err
	}
	bar.reset()
	// 显式关掉 CDN：这条路径就是要走主 DC（不带 cdn_supported 重新请求），
	// 不能依赖「NewDownloader 默认 allowCDN 为 nil」这个实现细节。
	legacy := downloader.NewDownloader().WithAllowCDN(false).Download(client.API(), info.Location).
		WithVerify(true)
	if err := fetch(ctx, legacy, file, bar, threads); err != nil {
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
