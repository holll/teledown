package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The proof's path binds the media identity to one local filename. Its hash
// binds that evidence to the actual bytes, not other files of the same size.
// We deliberately do not trust an unmarked file or overwrite an existing proof.
func downloadProofPath(target, kind string, mediaID int64, variant string, size int64) string {
	key := fmt.Sprintf("%q:%q:%d:%q:%d", filepath.Base(target), kind, mediaID, variant, size)
	return filepath.Join(filepath.Dir(target), fmt.Sprintf(".teledown-%x.media-v1", sha256.Sum256([]byte(key))))
}

type downloadHashReader struct {
	ctx context.Context
	io.Reader
}

func (r downloadHashReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func fingerprintDownload(ctx context.Context, f *os.File, info os.FileInfo) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	h := sha256.New()
	n, err := io.Copy(h, downloadHashReader{ctx, io.NewSectionReader(f, 0, info.Size())})
	if err != nil {
		return sum, err
	}
	current, err := f.Stat()
	if err != nil {
		return sum, err
	}
	if n != info.Size() || !sameDownloadFileInfo(info, current) {
		return sum, fmt.Errorf("计算校验和时下载文件已被修改")
	}
	h.Sum(sum[:0])
	return sum, ctx.Err()
}

func sameDownloadFileInfo(a, b os.FileInfo) bool {
	return a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func readDownloadProof(root *os.Root, path string) ([sha256.Size]byte, bool) {
	var want [sha256.Size]byte
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != sha256.Size {
		return want, false
	}
	f, err := root.Open(path)
	if err != nil {
		return want, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameDownloadFileInfo(info, opened) {
		return want, false
	}
	_, err = io.ReadFull(f, want[:])
	return want, err == nil
}

func matchesPublishedDownload(ctx context.Context, root *os.Root, target, proof string, info os.FileInfo, size int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if proof == "" || !info.Mode().IsRegular() || size > 0 && info.Size() != size {
		return false, nil
	}
	want, valid := readDownloadProof(root, proof)
	if !valid {
		return false, nil
	}
	f, err := root.Open(target)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameDownloadFileInfo(info, opened) {
		return false, nil
	}
	got, err := fingerprintDownload(ctx, f, opened)
	if err != nil || got != want {
		return false, ctx.Err()
	}
	current, err := root.Lstat(target)
	return err == nil && sameDownloadFileInfo(opened, current), ctx.Err()
}

func writeDownloadProof(root *os.Root, path string, sum [sha256.Size]byte) error {
	stage := filepath.Join(filepath.Dir(path), ".teledown-"+rand.Text()+".media-new")
	f, err := root.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(stage)
	defer f.Close()
	if _, err := f.Write(sum[:]); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A stale or user-owned marker isn't permission to replace it.
	return root.Link(stage, path)
}

func runNonResumeDownload(ctx context.Context, root *os.Root, target, proof string, size int64, link func(string, string) error, receive func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
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
	if err := probeDownloadLink(root, tmp, target, link); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := receive(file); err != nil {
		return err
	}
	return publishDownload(ctx, root, file, tmp, target, proof, size)
}

// Probe the exact inode and directory used by publication without touching
// the final name or requesting any Telegram file bytes.
func probeDownloadLink(root *os.Root, source, target string, link func(string, string) error) error {
	probe := filepath.Join(filepath.Dir(target), ".teledown-"+rand.Text()+".link-check")
	if err := link(source, probe); err != nil {
		return fmt.Errorf("保存目录不支持硬链接发布（未开始传输）: %w", err)
	}
	if err := root.Remove(probe); err != nil {
		return fmt.Errorf("清理硬链接探测文件失败（未开始传输）: %w", err)
	}
	return nil
}

func publishDownload(ctx context.Context, root *os.Root, file *os.File, tmp, target, proof string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	written, err := file.Stat()
	if err != nil {
		return fmt.Errorf("检查下载大小失败: %w", err)
	}
	if size > 0 && written.Size() != size {
		return fmt.Errorf("下载大小不符：实际 %d，预期 %d", written.Size(), size)
	}
	var sum [sha256.Size]byte
	if proof != "" {
		sum, err = fingerprintDownload(ctx, file, written)
		if err != nil {
			return fmt.Errorf("计算下载校验和失败: %w", err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("同步下载数据失败: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	source, err := root.Lstat(tmp)
	if err != nil {
		return fmt.Errorf("检查临时文件失败: %w", err)
	}
	// Some filesystems (including Windows) finalize write timestamps on
	// close, so check inode identity and size rather than the timestamp here.
	if !source.Mode().IsRegular() || !os.SameFile(written, source) || source.Size() != written.Size() {
		return fmt.Errorf("发布前下载临时文件已被更改")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Link(tmp, target); err != nil {
		return fmt.Errorf("保存下载文件失败（不会覆盖现有文件）: %w", err)
	}
	// Linking the final name is the commit point. A proof failure is safe:
	// keep the downloaded media, warn, and do not deduplicate it on rerun.
	if proof != "" {
		if err := writeDownloadProof(root, proof, sum); err != nil {
			fmt.Printf("无法记录媒体身份 %s（下次可能重新下载）: %v\n", target, err)
		}
	}
	return nil
}
