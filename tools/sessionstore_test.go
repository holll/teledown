package tools

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/peers"
)

const testChannelID = 1965235618

var channelKey = peers.Key{Prefix: "channel_", ID: testChannelID}

func newTestStore(t *testing.T) *SessionStore {
	t.Helper()
	return openTestStore(t, filepath.Join(t.TempDir(), "account.teledown.sqlite3"))
}

func openTestStore(t *testing.T, path string) *SessionStore {
	t.Helper()
	s, err := OpenSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func reopen(t *testing.T, s *SessionStore) *SessionStore {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return openTestStore(t, s.path)
}

func TestSessionStoreOpaquePayload(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("empty store: LoadSession() error = %v, want ErrNotFound", err)
	}
	if err := s.StoreSession(ctx, []byte("first payload")); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 0xff, 0xfe, 'x', 0}
	if err := s.StoreSession(ctx, payload); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	if got, err := s.LoadSession(ctx); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("LoadSession() = %x, %v; want %x", got, err, payload)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gotd_session`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("session rows = %d, %v; want 1", rows, err)
	}
	if err := s.StoreSession(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadSession(ctx); len(got) != 0 || err != nil {
		t.Fatalf("empty payload = %x, %v; want stored empty payload", got, err)
	}
}

func TestSessionStoreGotdLoader(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	want := session.Data{
		Config:           session.Config{ThisDC: 5, TestMode: true},
		DC:               5,
		AuthKey:          bytes.Repeat([]byte{0x4a}, 256),
		AuthKeyID:        []byte{1, 2, 3, 4, 5, 6, 7, 8},
		Salt:             183579926430176377,
		AuthKeyCreatedAt: 123,
		DCs:              map[int]session.DCKey{2: {AuthKey: []byte{9}, Authorized: true, Salt: 987}},
		ServerTimeOffset: 2 * time.Second,
		UserID:           42,
		Bot:              true,
	}
	if err := (&session.Loader{Storage: s}).Save(ctx, &want); err != nil {
		t.Fatal(err)
	}
	got, err := (&session.Loader{Storage: reopen(t, s)}).Load(ctx)
	if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("session.Loader did not preserve all fields: %v", err)
	}
}

func TestSessionStorePeerCache(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	userKey := peers.Key{Prefix: prefixUsers, ID: channelKey.ID}
	chatKey := peers.Key{Prefix: prefixChats, ID: channelKey.ID}
	for key, hash := range map[peers.Key]int64{channelKey: -100, userKey: 777, chatKey: 0} {
		if _, found, err := s.Find(ctx, key); err != nil || found {
			t.Fatalf("empty peer cache lookup = %v, %v", found, err)
		}
		if err := s.Save(ctx, key, peers.Value{AccessHash: hash}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(ctx, channelKey, peers.Value{AccessHash: -200}); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	for key, hash := range map[peers.Key]int64{channelKey: -200, userKey: 777, chatKey: 0} {
		if got, found, err := s.Find(ctx, key); err != nil || !found || got.AccessHash != hash {
			t.Errorf("Find(%+v) = %+v, %v, %v; want hash %d", key, got, found, err, hash)
		}
	}
}

func TestSessionStorePhoneMapping(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	userKey := peers.Key{Prefix: prefixUsers, ID: 42}
	otherKey := peers.Key{Prefix: prefixUsers, ID: 43}
	const phone = "001234"
	if err := s.SavePhone(ctx, phone, userKey); err != nil {
		t.Fatal(err)
	}
	if key, _, found, err := s.FindPhone(ctx, phone); err != nil || found || key != userKey {
		t.Fatalf("phone without peer = %+v, %v, %v", key, found, err)
	}
	for key, value := range map[peers.Key]int64{userKey: 555, otherKey: 777} {
		if err := s.Save(ctx, key, peers.Value{AccessHash: value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SavePhone(ctx, "1234", otherKey); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePhone(ctx, "1234", userKey); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePhone(ctx, phone, otherKey); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	for _, tc := range []struct {
		phone string
		key   peers.Key
		hash  int64
	}{{phone, otherKey, 777}, {"1234", userKey, 555}} {
		key, value, found, err := s.FindPhone(ctx, tc.phone)
		if err != nil || !found || key != tc.key || value.AccessHash != tc.hash {
			t.Errorf("FindPhone(%q) = %+v, %+v, %v, %v", tc.phone, key, value, found, err)
		}
	}
}

func TestSessionStoreContactsHash(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	if h, err := s.GetContactsHash(ctx); err != nil || h != 0 {
		t.Fatalf("initial hash = %d, %v; want 0", h, err)
	}
	if err := s.SaveContactsHash(ctx, 4242); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveContactsHash(ctx, -80); err != nil {
		t.Fatal(err)
	}
	if h, err := reopen(t, s).GetContactsHash(ctx); err != nil || h != -80 {
		t.Fatalf("reopened hash = %d, %v; want -80", h, err)
	}
}

func TestSessionStoreForget(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	userKey := peers.Key{Prefix: prefixUsers, ID: channelKey.ID}
	chatKey := peers.Key{Prefix: prefixChats, ID: channelKey.ID}
	otherKey := peers.Key{Prefix: prefixChannel, ID: channelKey.ID + 1}
	for _, key := range []peers.Key{channelKey, userKey, chatKey, otherKey} {
		if err := s.Save(ctx, key, peers.Value{AccessHash: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SavePhone(ctx, "01234", userKey); err != nil {
		t.Fatal(err)
	}
	s.ForgetChannel(channelKey.ID)
	for key, want := range map[peers.Key]bool{channelKey: false, userKey: true, chatKey: true, otherKey: true} {
		if _, found, err := s.Find(ctx, key); err != nil || found != want {
			t.Errorf("after ForgetChannel, Find(%+v) found=%v, err=%v; want found=%v", key, found, err, want)
		}
	}
	s.ForgetID(channelKey.ID)
	s = reopen(t, s)
	for key, want := range map[peers.Key]bool{channelKey: false, userKey: false, chatKey: false, otherKey: true} {
		if _, found, err := s.Find(ctx, key); err != nil || found != want {
			t.Errorf("after ForgetID, Find(%+v) found=%v, err=%v; want found=%v", key, found, err, want)
		}
	}
	if key, _, found, err := s.FindPhone(ctx, "01234"); err != nil || found || key != (peers.Key{}) {
		t.Errorf("forgotten phone lookup = %+v, %v, %v", key, found, err)
	}
}

func TestSessionStoreBrandedSchema(t *testing.T) {
	s := newTestStore(t)
	var brand, version int
	if err := s.db.QueryRow(`SELECT application_id, user_version FROM pragma_application_id(), pragma_user_version()`).Scan(&brand, &version); err != nil {
		t.Fatal(err)
	}
	if brand != 0x5444574e || version != 1 {
		t.Fatalf("SQLite brand/version = %x/%d, want TDWN/1", brand, version)
	}
	var tables string
	if err := s.db.QueryRow(`SELECT group_concat(name) FROM (SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name)`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != "contacts_state,gotd_session,peer_cache,peer_phones" {
		t.Fatalf("unexpected tables: %s", tables)
	}
}

func TestSessionStoreLeavesLegacyStoresUntouched(t *testing.T) {
	dir := t.TempDir()
	oldSQLite := filepath.Join(dir, "account.session")
	createForeignSQLite(t, oldSQLite, `CREATE TABLE version (version INTEGER); INSERT INTO version VALUES (7);
		CREATE TABLE sessions (dc_id INTEGER, auth_key BLOB); INSERT INTO sessions VALUES (5, x'0123');`)
	gotdJSON := filepath.Join(dir, "account.gotd.json")
	peerJSON := filepath.Join(dir, "account.peers.json")
	writeStorageFixture(t, gotdJSON, `{"Version":1,"Data":{"DC":5}}`)
	writeStorageFixture(t, peerJSON, `{"peers":{"channel_1965235618":42}}`)
	before := map[string][]byte{}
	for _, path := range []string{oldSQLite, gotdJSON, peerJSON} {
		before[path] = readStorageFixture(t, path)
		assertStoreRejected(t, path, "旧版")
	}
	// Even if an old database is copied to the new filename, it must not be adopted.
	copied := filepath.Join(dir, "copy.teledown.sqlite3")
	writeStorageFixture(t, copied, string(before[oldSQLite]))
	assertStoreRejected(t, copied, "不是 teledown Go 会话库")
	if !bytes.Equal(readStorageFixture(t, copied), before[oldSQLite]) {
		t.Fatal("unbranded database was modified")
	}

	s := openTestStore(t, filepath.Join(dir, "account.teledown.sqlite3"))
	ctx := t.Context()
	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("new database did not start with a fresh session: %v", err)
	}
	if _, found, err := s.Find(ctx, channelKey); err != nil || found {
		t.Fatalf("new database did not start with an empty peer cache: %v, %v", found, err)
	}
	if err := s.StoreSession(ctx, []byte("new storage")); err != nil {
		t.Fatal(err)
	}
	for path, content := range before {
		if !bytes.Equal(readStorageFixture(t, path), content) {
			t.Errorf("legacy file %s was modified", path)
		}
	}
}

func TestSessionStoreRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.teledown.sqlite3")
	createForeignSQLite(t, path, fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 42; CREATE TABLE future_data (value BLOB)", sessionApplicationID))
	before := readStorageFixture(t, path)
	assertStoreRejected(t, path, "版本 42")
	if !bytes.Equal(readStorageFixture(t, path), before) {
		t.Fatal("unsupported database was modified")
	}
}

func TestSessionStoreRejectsWrongSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.teledown.sqlite3")
	createForeignSQLite(t, path, fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = %d; CREATE TABLE unrelated (value BLOB)", sessionApplicationID, sessionSchemaVersion))
	assertStoreRejected(t, path, "gotd_session")
}

func TestSessionStoreRejectsCorruptAndEmptyFiles(t *testing.T) {
	for _, content := range []string{"", "not an SQLite database"} {
		path := filepath.Join(t.TempDir(), "existing.teledown.sqlite3")
		writeStorageFixture(t, path, content)
		assertStoreRejected(t, path, "SQLite")
		if string(readStorageFixture(t, path)) != content {
			t.Fatal("invalid file was modified")
		}
	}
}

func TestSessionStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows access is governed by directory ACLs")
	}
	s := newTestStore(t)
	if err := s.StoreSession(t.Context(), []byte("private payload")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database stat = %v, %v; want mode 0600", info, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path, 0o644); err != nil {
		t.Fatal(err)
	}
	assertStoreRejected(t, s.path, "0600")
}

func TestSessionStoreRejectsSymlinks(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "symlink.teledown.sqlite3")
	if err := os.Symlink(s.path, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlinks is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	assertStoreRejected(t, link, "符号链接")
	if err := os.Symlink(link, s.path+"-journal"); err != nil {
		t.Fatal(err)
	}
	assertStoreRejected(t, s.path, "辅助文件")
}

func TestSessionStoreEscapesFilename(t *testing.T) {
	// '?' is not permitted in Windows filenames; '#' and '%' are portable.
	path := filepath.Join(t.TempDir(), "中文 #100%", "account #%.teledown.sqlite3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at requested path: %v", err)
	}
	if err := s.SaveContactsHash(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	if got, err := reopen(t, s).GetContactsHash(t.Context()); err != nil || got != 42 {
		t.Fatalf("reopen hash = %d, %v; want 42", got, err)
	}
}

func TestSessionStoreCanceledOperations(t *testing.T) {
	s := newTestStore(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.SaveContactsHash(canceled, 42); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled write = %v", err)
	}
	if _, _, err := s.Find(canceled, channelKey); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled read = %v", err)
	}
	if got, err := s.GetContactsHash(t.Context()); err != nil || got != 0 {
		t.Errorf("canceled write changed hash: %d, %v", got, err)
	}
}

func createForeignSQLite(t *testing.T, path, ddl string) {
	t.Helper()
	writeStorageFixture(t, path, "")
	db, err := sql.Open("sqlite", sessionDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
}

func assertStoreRejected(t *testing.T, path, message string) {
	t.Helper()
	s, err := OpenSessionStore(path)
	if err == nil {
		_ = s.Close()
		t.Fatalf("unexpectedly opened %s", path)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("open error = %v; want %q", err, message)
	}
}

func writeStorageFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readStorageFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
