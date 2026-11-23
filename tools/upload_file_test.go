package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestUploadPartRetryClient(t *testing.T) {
	for _, req := range []bin.Encoder{
		&tg.UploadSaveFilePartRequest{FileID: 81, FilePart: 1, Bytes: []byte("data")},
		&tg.UploadSaveBigFilePartRequest{FileID: 81, FilePart: 2736, FileTotalParts: 3000, Bytes: []byte("data")},
	} {
		t.Run(fmt.Sprintf("%T", req), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
					calls++
					if in != req {
						t.Fatalf("分片请求被替换: %v", in)
					}
					if calls == 1 {
						return fmt.Errorf("rpcDoRequest: engine forcibly closed: %w", context.Canceled)
					}
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
					return nil
				}))
				c := uploadPartRetryClient{Client: api}
				var ok bool
				var err error
				switch part := req.(type) {
				case *tg.UploadSaveFilePartRequest:
					ok, err = c.UploadSaveFilePart(context.Background(), part)
				case *tg.UploadSaveBigFilePartRequest:
					ok, err = c.UploadSaveBigFilePart(context.Background(), part)
				}
				if !ok || err != nil || calls != 2 {
					t.Fatalf("分片结果 = %t, %v；共调用 %d 次", ok, err, calls)
				}
			})
		})
	}
}

func TestRetryUploadPartStops(t *testing.T) {
	closed := fmt.Errorf("engine forcibly closed: %w", context.Canceled)
	rpcErr := errors.New("FILE_PART_INVALID")
	for _, tc := range []struct {
		name       string
		sendErr    error
		wantErr    error
		wantCalls  int
		cancelWait bool
	}{
		{"耗尽重试", closed, closed, 3, false},
		{"非连接错误", rpcErr, rpcErr, 1, false},
		{"普通取消错误", context.Canceled, context.Canceled, 1, false},
		{"等待期间取消", closed, context.Canceled, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelWait {
					go func() {
						time.Sleep(500 * time.Millisecond)
						cancel()
					}()
				}
				calls := 0
				ok, err := retryUploadPart(ctx, func() (bool, error) {
					calls++
					return false, tc.sendErr
				})
				if ok || err != tc.wantErr || calls != tc.wantCalls {
					t.Fatalf("分片结果 = %t, %v；共调用 %d 次，预期错误 %v、调用 %d 次", ok, err, calls, tc.wantErr, tc.wantCalls)
				}
			})
		})
	}
}

func TestCheckUploadSize(t *testing.T) {
	const limit = int64(2_097_152_000)
	for _, tc := range []struct {
		name    string
		size    int64
		premium bool
		wantErr bool
	}{
		{"普通账号低于上限", limit - 1, false, false},
		{"普通账号等于上限", limit, false, false},
		{"普通账号超出一字节", limit + 1, false, true},
		{"Premium 不受普通账号上限限制", limit + 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkUploadSize(tc.size, tc.premium); (err != nil) != tc.wantErr {
				t.Fatalf("大小预检错误 = %v，预期报错 = %t", err, tc.wantErr)
			}
		})
	}
}

func TestVideoAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        float64
	}{
		{"fractional", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"0.5"}]}`, 0.5},
		{"fallback on N/A", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"N/A"}],"format":{"duration":"3.5"}}`, 3.5},
		{"nonfinite", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"NaN"}],"format":{"duration":"Inf"}}`, 0},
		{"attached cover only", `{"streams":[{"codec_type":"video","width":32,"height":24,"disposition":{"attached_pic":1}}],"format":{"duration":"5"}}`, 0},
		{"cover before video", `{"streams":[{"codec_type":"video","width":200,"height":200,"disposition":{"attached_pic":1}},{"codec_type":"video","width":32,"height":24,"duration":"2"}]}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result ffprobeResult
			if err := json.Unmarshal([]byte(tc.input), &result); err != nil {
				t.Fatal(err)
			}
			got := videoAttributes(result)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("expected invalid video, got %v", got)
				}
				return
			}
			if got == nil || got.Duration != tc.want || got.W != 32 || got.H != 24 {
				t.Fatalf("expected %gs, 32x24; got %v", tc.want, got)
			}
		})
	}
}

func TestDetectMIME(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"image", "\xff\xd8\xff", "image/jpeg"},
		{"image.MP4", "\xff\xd8\xff", "image/jpeg"},
		{"video", "\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00mp42", "video/mp4"},
		{"packed.mp4", "PK\x03\x04", "application/zip"},
		{"document.docx", "PK\x03\x04", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"table.CSV", "a,b\n1,2\n", "text/csv"},
		{"subs.SRT", "1\n00:00:00,000 --> 00:00:01,000\n字幕\n", "application/x-subrip"},
		{"notes", "just text", "text/plain"},
		{"unknown", "\x00\x01\x02", "application/octet-stream"},
		{"unknown.mp4", "\x00\x01\x02", "video/mp4"},
		{"empty.png", "", "image/png"},
		{"video.mkv", "\x1a\x45\xdf\xa3", "video/x-matroska"},
		{"video.webm", "\x1a\x45\xdf\xa3", "video/webm"},
		{"ebml", "\x1a\x45\xdf\xa3", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.header), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := detectMIME(path); got != tc.want {
				t.Fatalf("detectMIME(%q) = %q，期望 %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestCollectFilesSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.txt")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	got, isDir, err := collectFiles(dir)
	if err != nil || !isDir || !slices.Equal(got, []string{file}) {
		t.Fatalf("collectFiles directory = %v, %v, %v", got, isDir, err)
	}
	if _, _, err := collectFiles(link); err == nil {
		t.Fatal("explicit symlink must not be treated as uploadable")
	}
}
