package tools

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/peers"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动，不依赖 cgo。
)

const (
	sessionApplicationID = 0x5444574e // TDWN
	sessionSchemaVersion = 1
	// gotd peers 的键前缀；缓存直接使用该字符串，不转换 ID。
	prefixChannel = "channel_"
	prefixUsers   = "users_"
	prefixChats   = "chats_"
)

const sessionSchema = `
CREATE TABLE gotd_session (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    payload BLOB NOT NULL
) STRICT;
CREATE TABLE peer_cache (
    id INTEGER NOT NULL,
    prefix TEXT NOT NULL,
    access_hash INTEGER NOT NULL,
    PRIMARY KEY (id, prefix)
) STRICT;
CREATE TABLE peer_phones (
    phone TEXT PRIMARY KEY NOT NULL,
    id INTEGER NOT NULL,
    prefix TEXT NOT NULL
) STRICT;
CREATE INDEX peer_phones_by_peer ON peer_phones (id, prefix);
CREATE TABLE contacts_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    hash INTEGER NOT NULL
) STRICT;`

// SessionStore 在一个独立的 SQLite 文件中同时实现 gotd 的会话存储与 peer 缓存。
// 同一个实例可由多个 goroutine 使用。
type SessionStore struct {
	path string
	db   *sql.DB
}

var (
	_ session.Storage = (*SessionStore)(nil)
	_ peers.Storage   = (*SessionStore)(nil)
)

// OpenSessionStore 新建或打开专用会话库；不导入、改写或接管其他格式的文件。
// 目录应由当前用户控制；Windows 上文件访问权限由目录的继承 ACL 决定。
func OpenSessionStore(path string) (*SessionStore, error) {
	if path == "" {
		return nil, errors.New("会话库路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析会话库路径失败: %w", err)
	}
	switch name := strings.ToLower(strings.TrimRight(filepath.Base(abs), ". ")); {
	case strings.HasSuffix(name, ".session"), strings.HasSuffix(name, ".gotd.json"), strings.HasSuffix(name, ".peers.json"):
		return nil, fmt.Errorf("拒绝使用旧版会话或缓存文件名 %s", path)
	}

	// 不让 SQLite 自己建库：这样文件从创建起就是私有的，且不会截断已有文件。
	// 同时确保 SQLite 不会使用已有的不安全的辅助文件。
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := privateFileInfo(abs + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("检查会话库辅助文件失败: %w", err)
		}
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	created := err == nil
	switch {
	case created:
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("关闭新会话库文件 %s 失败: %w", path, err)
		}
	case errors.Is(err, os.ErrExist):
		if err := verifySessionHeader(abs); err != nil {
			return nil, fmt.Errorf("拒绝打开会话库 %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("创建会话库 %s 失败: %w", path, err)
	}

	db, err := sql.Open("sqlite", sessionDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("打开会话库 %s 失败: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &SessionStore{path: abs, db: db}
	if created {
		err = s.init()
	} else {
		err = s.verifySchema()
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("打开会话库 %s 失败: %w", path, err)
	}
	return s, nil
}

func privateFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件（不允许符号链接）", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s 权限过宽，请将权限设为 0600", path)
	}
	return info, nil
}

// 检查 SQLite 文件头后再用 SQLite 打开：不让 SQLite 对未知库执行日志恢复等操作。
func verifySessionHeader(path string) error {
	info, err := privateFileInfo(path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("会话库文件已被替换")
	}
	var header [100]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("无法读取 SQLite 文件头: %w", err)
	}
	if string(header[:16]) != "SQLite format 3\x00" || binary.BigEndian.Uint32(header[68:72]) != sessionApplicationID {
		return errors.New("不是 teledown Go 会话库；旧版会话文件不会被导入")
	}
	if version := binary.BigEndian.Uint32(header[60:64]); version != sessionSchemaVersion {
		return fmt.Errorf("不支持 teledown Go 会话库版本 %d（需要版本 %d）", version, sessionSchemaVersion)
	}
	return nil
}

// sessionDSN requires an absolute path. Escape filename bytes (such as ? and #)
// before appending SQLite parameters, and use an empty URI authority on Windows.
func sessionDSN(abs string) string {
	path := filepath.ToSlash(abs)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_pragma=busy_timeout(5000)&_pragma=trusted_schema(OFF)"}
	return u.String()
}

func (s *SessionStore) init() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(sessionSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = %d", sessionApplicationID, sessionSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SessionStore) verifySchema() error {
	var applicationID, version int
	if err := s.db.QueryRow(`SELECT application_id, user_version FROM pragma_application_id(), pragma_user_version()`).Scan(&applicationID, &version); err != nil {
		return err
	}
	if applicationID != sessionApplicationID || version != sessionSchemaVersion {
		return errors.New("会话库标记或版本不匹配")
	}
	// 验证必需的表和列存在，但不读取或修改任何数据。
	rows, err := s.db.Query(`SELECT s.singleton, s.payload, p.id, p.prefix, p.access_hash,
		f.phone, f.id, f.prefix, c.singleton, c.hash
		FROM gotd_session s, peer_cache p, peer_phones f, contacts_state c LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}

// Close 关闭会话库。
func (s *SessionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// LoadSession returns the exact payload most recently written by gotd.
func (s *SessionStore) LoadSession(ctx context.Context) ([]byte, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM gotd_session WHERE singleton = 1`).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取会话失败: %w", err)
	}
	return payload, nil
}

// StoreSession saves the entire gotd payload without decoding or transforming it.
func (s *SessionStore) StoreSession(ctx context.Context, payload []byte) error {
	if payload == nil {
		payload = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO gotd_session (singleton, payload) VALUES (1, ?)
		ON CONFLICT(singleton) DO UPDATE SET payload = excluded.payload`, payload)
	if err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

// Save 实现 peers.Storage。
func (s *SessionStore) Save(ctx context.Context, key peers.Key, value peers.Value) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO peer_cache (id, prefix, access_hash) VALUES (?, ?, ?)
		ON CONFLICT(id, prefix) DO UPDATE SET access_hash = excluded.access_hash`, key.ID, key.Prefix, value.AccessHash)
	if err != nil {
		return fmt.Errorf("写入 peer 缓存失败: %w", err)
	}
	return nil
}

// Find 实现 peers.Storage。
func (s *SessionStore) Find(ctx context.Context, key peers.Key) (peers.Value, bool, error) {
	var value peers.Value
	err := s.db.QueryRowContext(ctx, `SELECT access_hash FROM peer_cache WHERE id = ? AND prefix = ?`,
		key.ID, key.Prefix).Scan(&value.AccessHash)
	if errors.Is(err, sql.ErrNoRows) {
		return peers.Value{}, false, nil
	}
	if err != nil {
		return peers.Value{}, false, fmt.Errorf("读取 peer 缓存失败: %w", err)
	}
	return value, true, nil
}

// SavePhone 保存按文本精确匹配的手机号映射；它可以先于对应 peer 写入。
func (s *SessionStore) SavePhone(ctx context.Context, phone string, key peers.Key) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO peer_phones (phone, id, prefix) VALUES (?, ?, ?)
		ON CONFLICT(phone) DO UPDATE SET id = excluded.id, prefix = excluded.prefix`, phone, key.ID, key.Prefix)
	if err != nil {
		return fmt.Errorf("写入手机号映射失败: %w", err)
	}
	return nil
}

// FindPhone 查询手机号映射及其关联的 peer 缓存。
func (s *SessionStore) FindPhone(ctx context.Context, phone string) (peers.Key, peers.Value, bool, error) {
	var key peers.Key
	var value peers.Value
	var hash sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT f.id, f.prefix, p.access_hash FROM peer_phones f
		LEFT JOIN peer_cache p ON p.id = f.id AND p.prefix = f.prefix WHERE f.phone = ?`, phone).
		Scan(&key.ID, &key.Prefix, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return peers.Key{}, peers.Value{}, false, nil
	}
	if err != nil {
		return peers.Key{}, peers.Value{}, false, fmt.Errorf("按手机号读取 peer 缓存失败: %w", err)
	}
	value.AccessHash = hash.Int64
	return key, value, hash.Valid, nil
}

// GetContactsHash 实现 peers.Storage。
func (s *SessionStore) GetContactsHash(ctx context.Context) (int64, error) {
	var hash int64
	err := s.db.QueryRowContext(ctx, `SELECT hash FROM contacts_state WHERE singleton = 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取联系人版本号失败: %w", err)
	}
	return hash, nil
}

// SaveContactsHash 实现 peers.Storage。
func (s *SessionStore) SaveContactsHash(ctx context.Context, hash int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO contacts_state (singleton, hash) VALUES (1, ?)
		ON CONFLICT(singleton) DO UPDATE SET hash = excluded.hash`, hash)
	if err != nil {
		return fmt.Errorf("写入联系人版本号失败: %w", err)
	}
	return nil
}

// ForgetChannel 只删除频道缓存，避免误删相同数字 ID 的用户或基础群。
func (s *SessionStore) ForgetChannel(id int64) {
	s.forgetPeer(id, prefixChannel)
}

// ForgetID 删除数字 ID 相同的所有 peer 缓存以及相关手机号映射。
func (s *SessionStore) ForgetID(id int64) {
	s.forgetPeer(id, "")
}

func (s *SessionStore) forgetPeer(id int64, prefix string) {
	if err := s.deletePeer(id, prefix); err != nil {
		fmt.Fprintf(os.Stderr, "提示：peer 缓存删除失败（不影响下载）: %v\n", err)
	}
}

func (s *SessionStore) deletePeer(id int64, prefix string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where, args := ` WHERE id = ?`, []any{id}
	if prefix != "" {
		where += ` AND prefix = ?`
		args = append(args, prefix)
	}
	if _, err := tx.Exec(`DELETE FROM peer_phones`+where, args...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM peer_cache`+where, args...); err != nil {
		return err
	}
	return tx.Commit()
}
