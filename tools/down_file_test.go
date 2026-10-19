package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func downloadTestClient(invoke telegram.InvokeFunc) *telegram.Client {
	return telegram.NewClient(1, "test", telegram.Options{
		AllowCDN: true,
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(func(tg.Invoker) telegram.InvokeFunc {
			return invoke
		})},
	})
}

func TestDownloadOnePreservesExistingTemporaryName(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("失败=%v", fail), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "a.mp4")
			existing := target + ".downloading"
			original := []byte("已有媒体不得覆盖")
			if err := os.WriteFile(existing, original, 0o600); err != nil {
				t.Fatal(err)
			}
			data := []byte("new media")
			client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				if fail {
					return tgerr.New(400, "LOCATION_INVALID")
				}
				if _, ok := input.(*tg.UploadGetFileRequest); !ok {
					return fmt.Errorf("意外请求 %T", input)
				}
				output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data}
				return nil
			})
			info := &mediaInfo{Name: "a.mp4", Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
			err := downloadOne(context.Background(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1)
			if (err != nil) != fail {
				t.Fatalf("下载错误 = %v，预期失败=%v", err, fail)
			}
			if got, err := os.ReadFile(existing); err != nil || !bytes.Equal(got, original) {
				t.Errorf("已有媒体被修改：内容=%q，错误=%v", got, err)
			}
			if !fail {
				if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
					t.Errorf("下载内容=%q，错误=%v", got, err)
				}
			} else if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("失败后不应有目标文件：%v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if fail {
				want = 1
			}
			if len(entries) != want {
				t.Errorf("临时文件未清理：%v", entries)
			}
		})
	}
}

func TestDownloadOneUsesConfiguredThreads(t *testing.T) {
	const threads = 3
	const partSize = 512 * 1024
	data := bytes.Repeat([]byte{42}, threads*partSize)
	var entered atomic.Int32
	ready := make(chan struct{})
	client := downloadTestClient(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		req, ok := input.(*tg.UploadGetFileRequest)
		if !ok {
			return fmt.Errorf("意外请求 %T", input)
		}
		if req.Offset < int64(len(data)) {
			if entered.Add(1) == threads {
				close(ready)
			}
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var chunk []byte
		if req.Offset < int64(len(data)) {
			end := min(req.Offset+int64(req.Limit), int64(len(data)))
			chunk = data[req.Offset:end]
		}
		output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: chunk}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target := filepath.Join(t.TempDir(), "parallel.mp4")
	info := &mediaInfo{Name: "parallel.mp4", Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(ctx, client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, threads); err != nil {
		t.Fatalf("未使用指定的并发数完成下载：%v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("并发下载内容不一致，长度=%d，错误=%v", len(got), err)
	}
}

func TestDownloadOneFallbackResetsTemporaryFile(t *testing.T) {
	data := []byte("fallback media")
	hash := sha256.Sum256(data)
	fallback := false
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.UploadGetFileRequest:
			if !fallback {
				if req.Offset == 0 {
					output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: make([]byte, req.Limit)}
					return nil
				}
				fallback = true
				return tgerr.New(400, "LOCATION_INVALID")
			}
			if req.Offset != 0 {
				return fmt.Errorf("降级后起始偏移不为零：%d", req.Offset)
			}
			output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data}
		case *tg.UploadGetFileHashesRequest:
			output.(*tg.FileHashVector).Elems = []tg.FileHash{{Offset: 0, Limit: len(data), Hash: hash[:]}}
		default:
			return fmt.Errorf("意外请求 %T", input)
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "fallback.mp4")
	info := &mediaInfo{Name: "fallback.mp4", Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(context.Background(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("降级后文件未正确重置：长度=%d，错误=%v", len(got), err)
	}
}

func TestDownloadOneRefreshesExpiredReference(t *testing.T) {
	data := []byte("refreshed media")
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.UploadGetFileRequest:
			loc := req.Location.(*tg.InputDocumentFileLocation)
			if !bytes.Equal(loc.FileReference, []byte("fresh")) {
				return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
			}
			output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data}
		case *tg.UploadGetFileHashesRequest:
			return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		case *tg.ChannelsGetMessagesRequest:
			output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
				&tg.Message{ID: 5, PeerID: &tg.PeerChannel{ChannelID: 7}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, Size: int64(len(data)), FileReference: []byte("fresh")}}},
			}}
		default:
			return fmt.Errorf("意外请求 %T", input)
		}
		return nil
	})
	dir := t.TempDir()
	target := filepath.Join(dir, "refresh.mp4")
	info := &mediaInfo{Name: "refresh.mp4", Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(context.Background(), client, &tg.InputPeerChannel{ChannelID: 7}, &tg.Message{ID: 5}, info, target, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("引用刷新后内容错误：%q，错误=%v", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("引用刷新后临时文件未清理：%v，错误=%v", entries, err)
	}
}

func TestDownloadReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch input.(type) {
		case *tg.MessagesGetHistoryRequest:
			output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
				&tg.Message{ID: 1, Date: 1700000000, PeerID: &tg.PeerChannel{ChannelID: 7},
					Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, Size: 10, MimeType: "video/mp4"}},
				},
			}}
		case *tg.UploadGetFileRequest:
			cancel()
			return ctx.Err()
		default:
			return fmt.Errorf("意外请求 %T", input)
		}
		return nil
	})
	peer := &Peer{Input: &tg.InputPeerChannel{ChannelID: 7}, Title: "test", ID: 7}
	if err := Download(ctx, client, peer, DownloadOptions{SavePath: dir, Threads: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消被吞掉：%v", err)
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			t.Errorf("取消后残留文件：%s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadReportsFailuresAndContinues(t *testing.T) {
	for _, failures := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("失败数=%d", failures), func(t *testing.T) {
			data := []byte("good file")
			var visited [3]bool
			client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				switch req := input.(type) {
				case *tg.MessagesGetHistoryRequest:
					var msgs []tg.MessageClass
					for id := 2; id >= 1; id-- {
						msgs = append(msgs, &tg.Message{ID: id, Date: 1700000000,
							PeerID: &tg.PeerChannel{ChannelID: 7},
							Media:  &tg.MessageMediaDocument{Document: &tg.Document{ID: int64(id), Size: int64(len(data)), MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("%d.mp4", id)}}}},
						})
					}
					output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: msgs}
				case *tg.UploadGetFileRequest:
					id := req.Location.(*tg.InputDocumentFileLocation).ID
					visited[id] = true
					if int(id) > 2-failures {
						return tgerr.New(400, "LOCATION_INVALID")
					}
					output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data}
				case *tg.UploadGetFileHashesRequest:
					return tgerr.New(400, "LOCATION_INVALID")
				default:
					return fmt.Errorf("意外请求 %T", input)
				}
				return nil
			})
			peer := &Peer{Input: &tg.InputPeerChannel{ChannelID: 7}, Title: "test", ID: 7}
			err := Download(context.Background(), client, peer, DownloadOptions{SavePath: t.TempDir(), Threads: 1})
			if (err != nil) != (failures > 0) {
				t.Errorf("汇总错误=%v，失败数=%d", err, failures)
			}
			if failures > 0 && err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%d", failures)) {
				t.Errorf("错误没有包含失败数：%v", err)
			}
			if failures > 0 && !tg.IsLocationInvalid(err) {
				t.Errorf("汇总错误未保留底层错误链：%v", err)
			}
			if !visited[1] || !visited[2] {
				t.Errorf("单文件失败后没有继续：%v", visited)
			}
		})
	}
}
