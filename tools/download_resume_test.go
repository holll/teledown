package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestDownloadResumeVerifiesOwnedPrefix(t *testing.T) {
	if !downloadResumeLocksSupported {
		t.Skip("resume locking unavailable")
	}
	const partSize = 4 * 1024
	data := append(bytes.Repeat([]byte{17}, partSize), bytes.Repeat([]byte{31}, partSize)...)
	size := int64(len(data))
	var hashes []tg.FileHash
	for offset := 0; offset < len(data); offset += partSize {
		sum := sha256.Sum256(data[offset : offset+partSize])
		hashes = append(hashes, tg.FileHash{Offset: int64(offset), Limit: partSize, Hash: sum[:]})
	}
	for _, test := range []struct {
		name     string
		prefix   int64
		corrupt  bool
		refresh  bool
		replace  bool
		wantRPCs []int64
	}{
		{name: "verified prefix", prefix: partSize, wantRPCs: []int64{partSize}},
		{name: "local corruption restarts", prefix: partSize, corrupt: true, wantRPCs: []int64{0, partSize}},
		{name: "complete state publishes without downloading", prefix: size},
		{name: "refresh applies to local verification and download", prefix: partSize, refresh: true, wantRPCs: []int64{partSize}},
		{name: "replacement during verification is terminal", prefix: partSize, refresh: true, replace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			peer := &tg.InputPeerChannel{ChannelID: 7}
			msg := &tg.Message{ID: 5}
			info := &mediaInfo{FileID: 1, Size: size, Location: &tg.InputDocumentFileLocation{ID: 1, FileReference: []byte("original")}}
			identity, ok := newDownloadResumeIdentity(peer, msg, info)
			if !ok {
				t.Fatal("missing resume identity")
			}
			const target = "test.mp4"
			state, err := openDownloadResume(root, target, identity)
			if err != nil {
				t.Fatal(err)
			}
			{
				defer state.close()
				local := bytes.Clone(data)
				if test.corrupt {
					local[0] ^= 1
				}
				// Even though all file bytes match in the non-corrupt case,
				// bytes beyond the checkpoint were not acknowledged.
				if _, err := state.data.Write(local); err != nil {
					t.Fatal(err)
				}
				if err := state.savePrefix(test.prefix); err != nil {
					t.Fatal(err)
				}
				state.close()
			}
			var offsets []int64
			var refreshes, verifyCalls int
			client := downloadTestClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.UploadGetFileHashesRequest:
					verifyCalls++
					if test.refresh && string(req.Location.(*tg.InputDocumentFileLocation).FileReference) != "fresh" {
						return tgerr.New(400, "FILE_REFERENCE_INVALID")
					}
					out.(*tg.FileHashVector).Elems = hashes[req.Offset/partSize:]
				case *tg.UploadGetFileRequest:
					if req.CDNSupported || test.refresh && string(req.Location.(*tg.InputDocumentFileLocation).FileReference) != "fresh" {
						return errors.New("resume must use the verified, refreshed direct path")
					}
					offsets = append(offsets, req.Offset)
					out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: data[req.Offset : req.Offset+int64(req.Limit)]}
				case *tg.ChannelsGetMessagesRequest:
					refreshes++
					id := info.FileID
					if test.replace {
						id++
					}
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{
						&tg.Message{ID: msg.ID, PeerID: &tg.PeerChannel{ChannelID: peer.ChannelID}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: id, Size: size, FileReference: []byte("fresh")}}},
					}}
				default:
					return fmt.Errorf("unexpected request %T", in)
				}
				return nil
			})
			transfer := newDownloadTransfer(1)
			transfer.resume = true
			err = downloadOneRoot(t.Context(), client, peer, msg, info, root, target, transfer)
			if test.replace {
				if !errors.Is(err, errDownloadMediaChanged) {
					t.Fatalf("expected terminal media mismatch, got %v", err)
				}
				if _, err := root.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected published file or error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got, err := root.ReadFile(target); err != nil || !bytes.Equal(got, data) {
					t.Fatalf("download differs: bytes=%d err=%v", len(got), err)
				}
			}
			if verifyCalls == 0 || !slices.Equal(offsets, test.wantRPCs) || (refreshes == 1) != test.refresh {
				t.Fatalf("unexpected requests: hash=%d getFile=%v refresh=%d", verifyCalls, offsets, refreshes)
			}
			for _, path := range []string{state.marker, state.part} {
				if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("state not removed: %s: %v", path, err)
				}
			}
		})
	}
}

func TestDownloadResumeCheckpointsOnCancellation(t *testing.T) {
	if !downloadResumeLocksSupported {
		t.Skip("resume locking unavailable")
	}
	const partSize = 4 * 1024
	part := bytes.Repeat([]byte{17}, partSize)
	sum := sha256.Sum256(part)
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := downloadTestClient(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.UploadGetFileHashesRequest:
			out.(*tg.FileHashVector).Elems = []tg.FileHash{
				{Offset: 0, Limit: partSize, Hash: sum[:]}, {Offset: partSize, Limit: partSize, Hash: sum[:]},
			}
		case *tg.UploadGetFileRequest:
			if req.Offset == 0 {
				out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: part}
				return nil
			}
			// Wait until the single writer has reached the first block on disk.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				parts, _ := root.Open(".")
				if parts != nil {
					entries, _ := parts.ReadDir(-1)
					_ = parts.Close()
					for _, ent := range entries {
						if info, err := ent.Info(); err == nil && info.Size() == partSize {
							cancel()
							return ctx.Err()
						}
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		default:
			return fmt.Errorf("unexpected request %T", in)
		}
		return nil
	})
	peer := &tg.InputPeerChannel{ChannelID: 7}
	msg := &tg.Message{ID: 5}
	info := &mediaInfo{FileID: 1, Size: 2 * partSize, Location: &tg.InputDocumentFileLocation{ID: 1}}
	transfer := newDownloadTransfer(1)
	transfer.resume = true
	if err := downloadOneRoot(ctx, client, peer, msg, info, root, "test.mp4", transfer); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected controlled cancellation, got %v", err)
	}
	identity, _ := newDownloadResumeIdentity(peer, msg, info)
	state, err := openDownloadResume(root, "test.mp4", identity)
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	if state.acknowledged != partSize {
		t.Fatalf("checkpoint = %d, want %d", state.acknowledged, partSize)
	}
}

func TestDownloadResumeRefusesConcurrentWriterAndForeignFiles(t *testing.T) {
	if !downloadResumeLocksSupported {
		t.Skip("resume locking unavailable")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	peer := &tg.InputPeerChannel{ChannelID: 7}
	msg := &tg.Message{ID: 5}
	info := &mediaInfo{FileID: 1, Size: 4096, Location: &tg.InputDocumentFileLocation{ID: 1}}
	identity, _ := newDownloadResumeIdentity(peer, msg, info)
	const target = "test.mp4"
	first, err := openDownloadResume(root, target, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	var called atomic.Bool
	client := downloadTestClient(func(_ context.Context, _ bin.Encoder, _ bin.Decoder) error {
		called.Store(true)
		return errors.New("unexpected RPC")
	})
	transfer := newDownloadTransfer(1)
	transfer.resume = true
	if err := downloadOneRoot(t.Context(), client, peer, msg, info, root, target, transfer); err == nil || called.Load() {
		t.Fatalf("writer entered an owned checkpoint: err=%v called=%v", err, called.Load())
	}
	first.close()

	if err := root.Rename(first.part, "original"); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("not a teledown part")
	if err := root.WriteFile(first.part, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := downloadOneRoot(t.Context(), client, peer, msg, info, root, target, transfer); err == nil || called.Load() {
		t.Fatalf("writer accepted a replacement part: err=%v called=%v", err, called.Load())
	}
	if got, err := root.ReadFile(first.part); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign part was modified: %q %v", got, err)
	}
	if err := root.WriteFile(target, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := downloadOneRoot(t.Context(), client, peer, msg, info, root, target, transfer); !errors.Is(err, os.ErrExist) {
		t.Fatalf("writer accepted an existing destination: %v", err)
	}
	if got, err := root.ReadFile(target); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("destination was modified: %q %v", got, err)
	}
}
