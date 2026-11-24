package tools

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const (
	downloadResumeAlign         = 4 * 1024 // gotd WithOffset alignment in verified mode.
	downloadCheckpointInterval  = 8 * 1024 * 1024
	downloadCheckpointRecordLen = 16
)

type downloadResumeIdentity struct {
	PeerKind   string
	PeerID     int64
	MessageID  int
	DocumentID int64
	Size       int64
}

func newDownloadResumeIdentity(peer tg.InputPeerClass, msg *tg.Message, info *mediaInfo) (downloadResumeIdentity, bool) {
	loc, ok := info.Location.(*tg.InputDocumentFileLocation)
	if !ok || loc.ThumbSize != "" || loc.ID == 0 || info.Size <= 0 || info.IsSticker || msg.ID <= 0 {
		return downloadResumeIdentity{}, false
	}
	id := downloadResumeIdentity{MessageID: msg.ID, DocumentID: loc.ID, Size: info.Size}
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		id.PeerKind, id.PeerID = "channel", p.ChannelID
	case *tg.InputPeerChat:
		id.PeerKind, id.PeerID = "chat", p.ChatID
	case *tg.InputPeerUser:
		id.PeerKind, id.PeerID = "user", p.UserID
	case *tg.InputPeerSelf:
		if p, ok := msg.PeerID.(*tg.PeerUser); ok {
			id.PeerKind, id.PeerID = "user", p.UserID
		}
	}
	return id, id.PeerID > 0
}

type downloadResumeOwner struct {
	Version  int
	Identity downloadResumeIdentity
	Token    string
	FileKey  string
}

type downloadResume struct {
	root         *os.Root
	owner, data  *os.File
	marker, part string
	dataInfo     os.FileInfo
	recordAt     int64
	acknowledged int64
}

// The owner is keyed by stable media identity inside the destination directory
// (not by a case-sensitive spelling of the filename). Its random part name is
// only trusted after reading an exact identity match under the owner's lock.
func downloadResumeMarker(target string, id downloadResumeIdentity) string {
	key, _ := json.Marshal(id) // fixed fields consisting of integers and internal strings
	return filepath.Join(filepath.Dir(target), fmt.Sprintf(".teledown-%x.resume", sha256.Sum256(key)))
}

func downloadResumePart(marker, token string) string {
	return filepath.Join(filepath.Dir(marker), ".teledown-"+token+".resume-part")
}

func downloadResumeOwnerCreate(root *os.Root, marker string, id downloadResumeIdentity) error {
	owner := downloadResumeOwner{Version: 1, Identity: id, Token: rand.Text()}
	part := downloadResumePart(marker, owner.Token)
	data, err := root.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keepPart := false
	defer func() {
		if !keepPart {
			_ = root.Remove(part)
		}
	}()
	owner.FileKey, err = downloadResumeFileKey(data)
	if err != nil {
		_ = data.Close()
		return err
	}
	if err := data.Sync(); err != nil {
		_ = data.Close()
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}

	stage := filepath.Join(filepath.Dir(marker), ".teledown-"+rand.Text()+".resume-new")
	f, err := root.OpenFile(stage, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(stage)
	if err := json.NewEncoder(f).Encode(owner); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Publish the complete owner in one no-replace operation. A concurrent
	// creator can only open the installed owner, never a half-written header.
	if err := root.Link(stage, marker); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	keepPart = true
	return nil
}

func openDownloadResumeFile(root *os.Root, path string) (*os.File, error) {
	fi, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("断点续传文件不是普通文件: %s", path)
	}
	return root.OpenFile(path, os.O_RDWR, 0)
}

func checkDownloadResumeFile(root *os.Root, path string, f *os.File, key string) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	named, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(fi, named) {
		return nil, fmt.Errorf("断点续传文件已被替换: %s", path)
	}
	actual, err := downloadResumeFileKey(f)
	if err != nil {
		return nil, err
	}
	if key != "" && actual != key {
		return nil, fmt.Errorf("断点续传文件不属于当前下载: %s", path)
	}
	return fi, nil
}

func openDownloadResume(root *os.Root, target string, id downloadResumeIdentity) (_ *downloadResume, err error) {
	marker := downloadResumeMarker(target, id)
	owner, err := openDownloadResumeFile(root, marker)
	if errors.Is(err, os.ErrNotExist) {
		if err := downloadResumeOwnerCreate(root, marker, id); err != nil {
			return nil, err
		}
		owner, err = openDownloadResumeFile(root, marker)
	}
	if err != nil {
		return nil, err
	}
	state := &downloadResume{root: root, owner: owner, marker: marker}
	defer func() {
		if err != nil {
			state.close()
		}
	}()
	if err := lockDownloadResume(owner); err != nil {
		return nil, fmt.Errorf("无法独占断点续传记录（可能有其他进程正在下载）: %w", err)
	}
	// A caller may have opened the old inode just before the previous owner
	// removed the marker. Recheck the name after locking before using it.
	if _, err := checkDownloadResumeFile(root, marker, owner, ""); err != nil {
		return nil, err
	}
	if err := requireUnpublishedDownload(root, target); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(owner, 1024)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("读取断点续传记录失败: %w", err)
	}
	var record downloadResumeOwner
	if err := json.Unmarshal(header, &record); err != nil {
		return nil, fmt.Errorf("断点续传记录格式不正确: %w", err)
	}
	if record.Version != 1 || record.Identity != id || record.FileKey == "" || !validDownloadResumeToken(record.Token) {
		return nil, errors.New("断点续传记录与当前媒体不匹配")
	}
	state.recordAt = int64(len(header))
	var buf [downloadCheckpointRecordLen]byte
	if _, err := io.ReadFull(reader, buf[:]); err == nil {
		n := binary.LittleEndian.Uint64(buf[:8])
		if ^n == binary.LittleEndian.Uint64(buf[8:]) && n <= uint64(id.Size) {
			state.acknowledged = int64(n)
		}
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}

	state.part = downloadResumePart(marker, record.Token)
	state.data, err = openDownloadResumeFile(root, state.part)
	if err != nil {
		return nil, err
	}
	state.dataInfo, err = checkDownloadResumeFile(root, state.part, state.data, record.FileKey)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func validDownloadResumeToken(s string) bool {
	return len(s) >= 26 && len(s) <= 128 && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7')
	}) == -1
}

func requireUnpublishedDownload(root *os.Root, target string) error {
	_, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("保存下载文件失败（不会覆盖现有文件）: %w", os.ErrExist)
	}
	return err
}

func (s *downloadResume) close() {
	if s.data != nil {
		_ = s.data.Close()
	}
	_ = s.owner.Close() // closing releases the process-owned kernel lock
}

func (s *downloadResume) remove() error {
	_ = s.data.Close()
	// Check the part by inode even after publication (then it legitimately has
	// two links). Never remove a path that no longer names our owned file.
	named, err := s.root.Lstat(s.part)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if !named.Mode().IsRegular() || !os.SameFile(s.dataInfo, named) {
			return fmt.Errorf("断点续传文件已被替换: %s", s.part)
		}
		if err := s.root.Remove(s.part); err != nil {
			return err
		}
	}
	// Unlink while still holding the lock; waiters must recheck inode equality.
	if _, err := checkDownloadResumeFile(s.root, s.marker, s.owner, ""); err != nil {
		return err
	}
	return s.root.Remove(s.marker)
}

func (s *downloadResume) savePrefix(prefix int64) error {
	if prefix == s.acknowledged {
		return nil
	}
	// Sync payload before certifying the contiguous write prefix. A torn or
	// absent record is treated as zero; every retained byte is still verified
	// against Telegram before being counted or published.
	if err := s.data.Sync(); err != nil {
		return err
	}
	var buf [downloadCheckpointRecordLen]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(prefix))
	binary.LittleEndian.PutUint64(buf[8:], ^uint64(prefix))
	if _, err := s.owner.WriteAt(buf[:], s.recordAt); err != nil {
		return err
	}
	if err := s.owner.Sync(); err != nil {
		return err
	}
	s.acknowledged = prefix
	return nil
}

func downloadWithResume(ctx context.Context, client *telegram.Client, root *os.Root, target string, info *mediaInfo, ref *downloadReference, bar *progressBar, transfer downloadTransfer, id downloadResumeIdentity) (err error) {
	if !downloadResumeLocksSupported {
		return errors.New("当前系统不支持独占断点续传")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireUnpublishedDownload(root, target); err != nil {
		return err
	}
	state, err := openDownloadResume(root, target, id)
	if err != nil {
		return fmt.Errorf("打开断点续传状态失败: %w", err)
	}
	defer state.close()
	defer func() {
		if err == nil || errors.Is(err, errDownloadMediaChanged) {
			if cleanupErr := state.remove(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("清理断点续传状态失败: %w", cleanupErr))
			}
		}
	}()

	// Use the direct DC for both Verify and download: v0.166.1 Verify uses
	// master upload.getFileHashes without preparing a CDN session. Retain the
	// same refreshable builder so repaired references also apply to chunk RPCs.
	builder := transfer.builder(client, info, ref, true).WithVerify(true)
	candidate := min(state.acknowledged, state.dataInfo.Size())
	verified, err := builder.Verify(ctx, state.data, candidate)
	if err := downloadResult(ctx, ref, err); err != nil {
		return fmt.Errorf("验证本地断点失败: %w", err)
	}
	if verified < id.Size {
		verified -= verified % downloadResumeAlign
	}
	if err := state.data.Truncate(verified); err != nil {
		return fmt.Errorf("截断未经验证的下载数据失败: %w", err)
	}
	if err := state.savePrefix(verified); err != nil {
		return fmt.Errorf("保存已验证的下载断点失败: %w", err)
	}
	if verified > 0 {
		bar.Add(verified)
	}

	if verified < id.Size {
		sink := downloadSink{file: state.data, bar: bar, size: id.Size}
		if verified > 0 {
			sink.ranges = []byteRange{{start: 0, end: verified}}
		}
		sink.checkpoint = func(prefix int64) error {
			if prefix-state.acknowledged >= downloadCheckpointInterval {
				return state.savePrefix(prefix)
			}
			return nil
		}
		_, runErr := builder.WithOffset(verified).Parallel(ctx, &sink)
		runErr = downloadResult(ctx, ref, runErr)
		if errors.Is(runErr, errDownloadMediaChanged) {
			return runErr
		}
		if runErr == nil {
			runErr = sink.complete()
		}
		// Also checkpoint on a graceful failure/cancel; only the observed
		// contiguous ranges are eligible, never the apparent file length.
		if err := state.savePrefix(sink.prefix()); err != nil {
			return errors.Join(runErr, fmt.Errorf("保存下载断点失败: %w", err))
		}
		if runErr != nil {
			return runErr
		}
	}
	if err := state.data.Sync(); err != nil {
		return err
	}
	if _, err := checkDownloadResumeFile(root, state.part, state.data, ""); err != nil {
		return err
	}
	return publishDownload(ctx, root, state.data, state.part, target, id.Size)
}
