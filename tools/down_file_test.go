package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type testCDNProvider func(context.Context, int, int64) (downloader.CDN, io.Closer, error)

func (f testCDNProvider) CDN(ctx context.Context, dc int, max int64) (downloader.CDN, io.Closer, error) {
	return f(ctx, dc, max)
}

func TestDownloadCDNUsesWaitMiddleware(t *testing.T) {
	var calls int
	p := downloadCDNProvider{
		CDNProvider: testCDNProvider(func(context.Context, int, int64) (downloader.CDN, io.Closer, error) {
			return tg.NewClient(telegram.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
				t.Fatal("terminal wait must stop the raw CDN invoker")
				return nil
			})), nil, nil
		}),
		wait: telegram.MiddlewareFunc(func(tg.Invoker) telegram.InvokeFunc {
			return func(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
				if _, ok := input.(*tg.UploadGetCDNFileRequest); !ok {
					t.Fatalf("unexpected request %T", input)
				}
				calls++
				return ErrTransferWaitLimit
			}
		}),
	}
	cdn, _, err := p.CDN(t.Context(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cdn.UploadGetCDNFile(t.Context(), &tg.UploadGetCDNFileRequest{})
	if calls != 1 || !errors.Is(err, ErrTransferWaitLimit) || shouldFallbackDownload(t.Context(), err) {
		t.Fatalf("calls=%d err=%v, expected terminal wait without fallback", calls, err)
	}
}

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
			want := 3 // existing file + completed media + proof
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
		if req.Offset >= int64(len(data)) {
			return fmt.Errorf("不应向服务端探测已知文件的 EOF：%d", req.Offset)
		}
		end := min(req.Offset+int64(req.Limit), int64(len(data)))
		output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data[req.Offset:end]}
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

func TestDownloadOneEOFProbeDependsOnExactSize(t *testing.T) {
	const partSize = 512 * 1024
	data := bytes.Repeat([]byte{42}, partSize)
	for _, tc := range []struct {
		name     string
		location tg.InputFileLocationClass
		size     int64
		want     int
	}{
		{"known document", &tg.InputDocumentFileLocation{ID: 1}, partSize, 1},
		{"unknown document", &tg.InputDocumentFileLocation{ID: 1}, 0, 2},
		{"photo", &tg.InputPhotoFileLocation{ID: 1, ThumbSize: "x"}, partSize, 2},
		{"unknown photo", &tg.InputPhotoFileLocation{ID: 1, ThumbSize: "x"}, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				req, ok := input.(*tg.UploadGetFileRequest)
				if !ok {
					return fmt.Errorf("unexpected request %T", input)
				}
				requests++
				if req.Limit != partSize || req.Offset != int64((requests-1)*partSize) {
					return fmt.Errorf("unexpected request: offset=%d limit=%d", req.Offset, req.Limit)
				}
				var chunk []byte
				if req.Offset == 0 {
					chunk = data
				}
				output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: chunk}
				return nil
			})
			info := &mediaInfo{Name: "media", Size: tc.size, Location: tc.location}
			target := filepath.Join(t.TempDir(), "media")
			if err := downloadOne(t.Context(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1); err != nil {
				t.Fatal(err)
			}
			if requests != tc.want {
				t.Fatalf("sent %d RPCs, want %d", requests, tc.want)
			}
			if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("download differs: bytes=%d err=%v", len(got), err)
			}
		})
	}
}

func TestDownloadTransferKeepsWindowAcrossFilesAndAlignedTail(t *testing.T) {
	const partSize = 512 * 1024
	data := bytes.Repeat([]byte{42}, partSize+7)
	var requests atomic.Int32
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		req, ok := input.(*tg.UploadGetFileRequest)
		if !ok {
			return fmt.Errorf("unexpected request %T", input)
		}
		if req.Offset < 0 || req.Offset >= int64(len(data)) || req.Offset%partSize != 0 || req.Limit != partSize {
			return fmt.Errorf("unexpected request: offset=%d limit=%d", req.Offset, req.Limit)
		}
		requests.Add(1)
		end := min(req.Offset+int64(req.Limit), int64(len(data)))
		output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: data[req.Offset:end]}
		return nil
	})
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	transfer := newDownloadTransfer(5)
	transfer.stats = new(TransferStats)
	if got := transfer.scheduler.Snapshot().Window; got != 4 {
		t.Fatalf("starting window = %d, want 4", got)
	}
	peer := &tg.InputPeerSelf{}
	for i := 1; i <= 3; i++ {
		info := &mediaInfo{Name: "media", Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: int64(i)}}
		target := fmt.Sprintf("file-%d", i)
		if err := downloadOneRoot(t.Context(), client, peer, &tg.Message{ID: i}, info, root, target, transfer); err != nil {
			t.Fatal(err)
		}
		if got, err := root.ReadFile(target); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("download differs: bytes=%d err=%v", len(got), err)
		}
	}
	if got := requests.Load(); got != 6 {
		t.Fatalf("sent %d RPCs, want 6", got)
	}
	if got := transfer.scheduler.Snapshot(); got.Window != 5 || got.InFlight != 0 || got.Waiting != 0 {
		t.Fatalf("shared scheduler snapshot = %+v, want window 5 and no outstanding work", got)
	}
	if window, peak := transfer.stats.window.Load(), transfer.stats.peak.Load(); window != 5 || peak != 5 {
		t.Fatalf("observed window/peak = %d/%d, want 5/5", window, peak)
	}
}

func TestDownloadSinkRejectsHolesAndOverlappingChunks(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "sink")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(8); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, 8} {
		w := downloadSink{file: file, bar: newProgressBar("media", size), size: size}
		if err := w.complete(); err == nil {
			t.Fatal("empty download accepted")
		}
		if _, err := w.WriteAt([]byte("gh"), 6); err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteAt([]byte("ab"), 0); err != nil {
			t.Fatal(err)
		}
		if err := w.complete(); err == nil {
			t.Fatal("accepted hole in download")
		}
		if _, err := w.WriteAt([]byte("bc"), 1); err == nil {
			t.Fatal("accepted overlapping range")
		}
		if _, err := w.WriteAt([]byte("cdef"), 2); err != nil {
			t.Fatal(err)
		}
		if err := w.complete(); err != nil {
			t.Fatal(err)
		}
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
				// The mock client has no configuration for this CDN DC, so
				// acquiring its CDN transport fails after the redirect.
				output.(*tg.UploadFileBox).File = &tg.UploadFileCDNRedirect{DCID: 999}
				return nil
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
	info := &mediaInfo{Name: "fallback.mp4", Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(context.Background(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("降级后文件未正确重置：长度=%d，错误=%v", len(got), err)
	}
}

func TestDownloadDoesNotFallbackOnOriginErrors(t *testing.T) {
	for _, failure := range []error{tgerr.New(400, "LOCATION_INVALID"), ErrTransferWaitLimit} {
		t.Run(failure.Error(), func(t *testing.T) {
			var getFiles, hashes int
			client := downloadTestClient(func(_ context.Context, req bin.Encoder, _ bin.Decoder) error {
				switch req.(type) {
				case *tg.UploadGetFileRequest:
					getFiles++
				case *tg.UploadGetFileHashesRequest:
					hashes++
				}
				return failure
			})
			target := filepath.Join(t.TempDir(), "media")
			info := &mediaInfo{Name: "media", Location: &tg.InputDocumentFileLocation{ID: 1}}
			if err := downloadOne(t.Context(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1); err == nil {
				t.Fatal("failure lost")
			}
			if getFiles != 1 || hashes != 0 {
				t.Fatalf("unexpected fallback: getFile=%d hashes=%d", getFiles, hashes)
			}
		})
	}
}

func TestDownloadCDNFallbackRejectsTerminalErrors(t *testing.T) {
	for _, err := range []error{ErrTransferWaitLimit, downloader.ErrHashMismatch, tgerr.New(420, "FLOOD_WAIT_3"), tgerr.New(400, "FILE_REFERENCE_0_EXPIRED")} {
		if shouldFallbackDownload(t.Context(), &downloadCDNError{cause: err}) {
			t.Errorf("terminal CDN error would be retried: %v", err)
		}
	}
	if shouldFallbackDownload(t.Context(), io.ErrShortWrite) {
		t.Error("local I/O error would be retried")
	}
	if !shouldFallbackDownload(t.Context(), &downloadCDNError{cause: io.ErrUnexpectedEOF}) {
		t.Error("CDN transport error would not be retried")
	}
}

func TestDownloadOneRefreshesStaleReference(t *testing.T) {
	for _, code := range []string{"FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID"} {
		t.Run(code, func(t *testing.T) { testDownloadOneRefreshesReference(t, code) })
	}
}

func testDownloadOneRefreshesReference(t *testing.T, code string) {
	t.Helper()
	data := []byte("refreshed media")
	var refreshes, staleRequests int
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.UploadGetFileRequest:
			loc := req.Location.(*tg.InputDocumentFileLocation)
			if !bytes.Equal(loc.FileReference, []byte("fresh")) {
				staleRequests++
				return tgerr.New(400, code)
			}
			output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: data}
		case *tg.ChannelsGetMessagesRequest:
			refreshes++
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
	info := &mediaInfo{Name: "refresh.mp4", FileID: 1, Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(context.Background(), client, &tg.InputPeerChannel{ChannelID: 7}, &tg.Message{ID: 5}, info, target, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("引用刷新后内容错误：%q，错误=%v", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("引用刷新后应只剩已发布的媒体及其证明：%v，错误=%v", entries, err)
	}
	if staleRequests != 1 || refreshes != 1 {
		t.Fatalf("stale requests=%d refreshes=%d; want exactly one of each", staleRequests, refreshes)
	}
}

func TestDownloadRefreshSharesLocationAndKeepsOtherParts(t *testing.T) {
	const partSize = 512 * 1024
	data := bytes.Repeat([]byte{17}, 3*partSize)
	var first, stale, refreshes atomic.Int32
	bothStale := make(chan struct{})
	client := downloadTestClient(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.UploadGetFileRequest:
			loc := req.Location.(*tg.InputDocumentFileLocation)
			if req.Offset == 0 {
				first.Add(1)
			} else if !bytes.Equal(loc.FileReference, []byte("fresh")) {
				if stale.Add(1) == 2 {
					close(bothStale)
				}
				select {
				case <-bothStale:
				case <-ctx.Done():
					return ctx.Err()
				}
				return tgerr.New(400, "FILE_REFERENCE_0_EXPIRED")
			}
			out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: data[req.Offset : req.Offset+int64(req.Limit)]}
		case *tg.ChannelsGetMessagesRequest:
			refreshes.Add(1)
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
				&tg.Message{ID: 5, PeerID: &tg.PeerChannel{ChannelID: 7}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, Size: int64(len(data)), FileReference: []byte("fresh")}}},
			}}
		default:
			return fmt.Errorf("unexpected RPC %T", in)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	target := filepath.Join(t.TempDir(), "media")
	info := &mediaInfo{FileID: 1, Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 1}}
	if err := downloadOne(ctx, client, &tg.InputPeerChannel{ChannelID: 7}, &tg.Message{ID: 5}, info, target, 3); err != nil {
		t.Fatal(err)
	}
	if first.Load() != 1 || stale.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("first part=%d stale=%d refreshes=%d", first.Load(), stale.Load(), refreshes.Load())
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("download differs: bytes=%d err=%v", len(got), err)
	}
}

func TestDownloadOneRejectsReplacedMedia(t *testing.T) {
	for name, replacement := range map[string]tg.MessageMediaClass{
		"new document ID": &tg.MessageMediaDocument{Document: &tg.Document{ID: 2, Size: 10}},
		"changed size":    &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, Size: 11}},
		"photo ID alias": &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1, Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "x", W: 1, H: 1, Size: 10},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			var downloadCount int
			client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				switch input.(type) {
				case *tg.UploadGetFileRequest:
					downloadCount++
					return tgerr.New(400, "FILE_REFERENCE_INVALID")
				case *tg.ChannelsGetMessagesRequest:
					output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
						&tg.Message{ID: 5, PeerID: &tg.PeerChannel{ChannelID: 7}, Media: replacement},
					}}
					return nil
				default:
					return fmt.Errorf("unexpected request %T", input)
				}
			})
			info := &mediaInfo{Name: "media", FileID: 1, Size: 10, Location: &tg.InputDocumentFileLocation{ID: 1}}
			dir := t.TempDir()
			err := downloadOne(t.Context(), client, &tg.InputPeerChannel{ChannelID: 7}, &tg.Message{ID: 5}, info, filepath.Join(dir, "media"), 1)
			if !errors.Is(err, errDownloadMediaChanged) || downloadCount != 1 {
				t.Fatalf("replaced media not rejected, err=%v download requests=%d", err, downloadCount)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("failed download left files: %v, %v", entries, err)
			}
		})
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	stdout := os.Stdout
	os.Stdout = w
	downloadErr := Download(ctx, client, peer, DownloadOptions{SavePath: dir, Threads: 1})
	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(downloadErr, context.Canceled) {
		t.Fatalf("取消被吞掉：%v", downloadErr)
	}
	if out, err := io.ReadAll(r); err != nil || len(out) != 0 {
		t.Fatalf("取消不应输出下载失败或完成：%q，读取错误=%v", out, err)
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
						msgs = append(msgs, &tg.Message{ID: id, Date: 1700000000 + id*40*86400,
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
			dir := t.TempDir()
			err := Download(context.Background(), client, peer, DownloadOptions{SavePath: dir, Threads: 1})
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
			channel := filepath.Join(dir, "test-7")
			entries, err := os.ReadDir(channel)
			if err != nil || len(entries) != 2*(2-failures) {
				t.Fatalf("频道目录应只包含成功下载的文件和证明：%v，错误=%v", entries, err)
			}
			for id := 1; id <= 2-failures; id++ {
				path := filepath.Join(channel, fmt.Sprintf("%d.mp4", id))
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
					t.Errorf("文件应直接保存在频道目录：%s，内容=%q，错误=%v", path, got, err)
				}
			}
		})
	}
}

func TestDownloadRejectsShortMediaAndDoesNotClobber(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "a.mp4")
			size := int64(5)
			if existing {
				size = 1
				if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			client := downloadTestClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.UploadGetFileRequest); !ok {
					return fmt.Errorf("unexpected %T", in)
				}
				out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: []byte("a")}
				return nil
			})
			info := &mediaInfo{Location: &tg.InputDocumentFileLocation{ID: 1}, Name: "a.mp4", Size: size}
			if err := downloadOne(context.Background(), client, &tg.InputPeerSelf{}, &tg.Message{ID: 1}, info, target, 1); err == nil {
				t.Fatal("expected rejection")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !existing && len(entries) != 0 {
				t.Fatalf("partial remained: %v", entries)
			}
			if existing {
				if b, err := os.ReadFile(target); err != nil || string(b) != "untouched" || len(entries) != 1 {
					t.Fatalf("files=%v contents=%q err=%v", entries, b, err)
				}
			}
		})
	}
}

func TestDownloadRejectsLinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	outer := t.TempDir()
	if err := os.Symlink(outer, filepath.Join(dir, "channel")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureDownloadDir(root, "channel"); err == nil {
		t.Fatal("directory link accepted")
	}
}

func TestDownloadKeepsSameNameAndSizeMediaDistinct(t *testing.T) {
	dir := t.TempDir()
	channel := filepath.Join(dir, "test-7")
	if err := os.Mkdir(channel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(channel, "same.mp4"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	const size = 6
	data := map[int64]string{31: "first!", 32: "second"}
	var requests atomic.Int32
	client := downloadTestClient(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.ChannelsGetMessagesRequest:
			var rows []tg.MessageClass
			for _, id := range []int{2, 1} {
				rows = append(rows, &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: 7},
					Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: int64(30 + id), Size: size, MimeType: "video/mp4",
						Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "same.mp4"}}}},
				})
			}
			output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: rows}
		case *tg.UploadGetFileRequest:
			id := req.Location.(*tg.InputDocumentFileLocation).ID
			requests.Add(1)
			output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: []byte(data[id])}
		default:
			return fmt.Errorf("unexpected RPC %T", input)
		}
		return nil
	})
	peer := &Peer{Input: &tg.InputPeerChannel{ChannelID: 7}, Title: "test", ID: 7}
	opt := DownloadOptions{SavePath: dir, Threads: 1, Range: MsgRange{Mode: RangeList, IDs: []int{2, 99, 1}}}
	run := func(wantRequests int32) {
		t.Helper()
		var missing *MissingMessageIDsError
		if err := Download(t.Context(), client, peer, opt); !errors.As(err, &missing) || len(missing.IDs) != 1 || missing.IDs[0] != 99 {
			t.Fatalf("expected CLI-visible missing ID after processing available rows, got %v", err)
		}
		if got := requests.Load(); got != wantRequests {
			t.Fatalf("file RPCs = %d, want %d", got, wantRequests)
		}
	}
	run(2) // neither legacy bytes nor the other media may suppress a transfer
	run(2) // reopen the root and skip exactly the two recorded media
	if err := os.WriteFile(filepath.Join(channel, "same(2).mp4"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(3) // same-size tampering invalidates only the changed media's proof
	for name, expected := range map[string]string{"same.mp4": "legacy", "same(2).mp4": "edited", "same(3).mp4": "second", "same(4).mp4": "first!"} {
		if got, err := os.ReadFile(filepath.Join(channel, name)); err != nil || string(got) != expected {
			t.Errorf("%s = %q, want %q, err=%v", name, got, expected, err)
		}
	}
}

func TestDownloadRefreshMissingMessageRetainsMediaChangedError(t *testing.T) {
	client := downloadTestClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch in.(type) {
		case *tg.UploadGetFileRequest:
			return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		case *tg.ChannelsGetMessagesRequest:
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 5}}}
			return nil
		default:
			return fmt.Errorf("unexpected RPC %T", in)
		}
	})
	info := &mediaInfo{Location: &tg.InputDocumentFileLocation{ID: 1}, FileID: 1, Size: 4}
	var missing *MissingMessageIDsError
	err := downloadOne(t.Context(), client, &tg.InputPeerChannel{ChannelID: 7}, &tg.Message{ID: 5}, info, filepath.Join(t.TempDir(), "media"), 1)
	if !errors.Is(err, errDownloadMediaChanged) || errors.As(err, &missing) {
		t.Fatalf("reference refresh should retain the changed-media error, got %v", err)
	}
}

func TestMatchesSenderFailClosed(t *testing.T) {
	if !matchesSender(nil, nil) || matchesSender(map[int64]struct{}{}, &tg.PeerUser{UserID: 7}) {
		t.Fatal("empty and nil filters conflated")
	}
	filter := map[int64]struct{}{-1000000000007: {}}
	if matchesSender(filter, nil) || matchesSender(filter, &tg.PeerUser{UserID: 7}) || !matchesSender(filter, &tg.PeerChannel{ChannelID: 7}) {
		t.Fatal("sender identity was not fail-closed")
	}
}
