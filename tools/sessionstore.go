package tools

// 本文件是会话与 peer 缓存的持久化：一个 Telethon 风格的 SQLite 库。
//
// 之前是两份 JSON——<账号标识>.gotd.json 存 auth key 与 DC，<账号标识>.peers.json 存
// access_hash（见 config.go），现在合并成一个 <账号标识>.session，表结构与 Telethon 的
// SQLiteSession 一致（https://github.com/LonamiWebs/Telethon/blob/master/telethon/sessions/sqlite.py）：
//
//	version       数据库版本，我们写 7
//	sessions      auth key 与 DC           ← 对应 gotd 的 session.Storage
//	entities      peer 的 access_hash      ← 对应 gotd 的 peers.Storage（也对应 Telethon 的实体缓存）
//	sent_files    文件引用缓存             ← 本工具不用，建出来只为表结构与 Telethon 一致
//	update_state  updates 游标             ← 同上
//	store_state   本工具自用               ← Telethon 没有放「联系人版本号」的地方，单独开一张表
//
// 版本号写 7 而不是最新的 8：Telethon 读到 7 < CURRENT_VERSION 会自动 alter 升级，
// 而老版本 Telethon 读到 8 会在解包 sessions 行时直接报错，写 7 两边都能用。
//
// entities 的主键用 Telethon 的「带标记 peer id」：用户 = +id，基础群 = -id，
// 频道/超级群 = -1000000000000 - id（即 TDLib 的编码，见 constant.TDLibPeerID）。
// gotd 的 peers.Key 用 Prefix 区分类型，两套编码在这里互转。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动：不引入 cgo，交叉编译不受影响
)

// sessionSchema 是建表语句，与 Telethon 逐一对应（多出的 store_state 见文件头说明）。
const sessionSchema = `
CREATE TABLE IF NOT EXISTS version (
    version integer primary key
);
CREATE TABLE IF NOT EXISTS sessions (
    dc_id integer primary key,
    server_address text,
    port integer,
    auth_key blob,
    takeout_id integer
);
CREATE TABLE IF NOT EXISTS entities (
    id integer primary key,
    hash integer not null,
    username text,
    phone integer,
    name text,
    date integer
);
CREATE TABLE IF NOT EXISTS sent_files (
    md5_digest blob,
    file_size integer,
    type integer,
    id integer,
    hash integer,
    primary key (md5_digest, file_size, type)
);
CREATE TABLE IF NOT EXISTS update_state (
    id integer primary key,
    pts integer,
    qts integer,
    date integer,
    seq integer
);
CREATE TABLE IF NOT EXISTS store_state (
    key text primary key,
    value integer
);`

// telethonVersion 是我们写入的数据库版本，理由见文件头。
const telethonVersion = 7

// contactsHashKey 是 store_state 里存联系人版本号的键。
const contactsHashKey = "contacts_hash"

// gotd 的 peers.Storage 用这三个前缀区分实体类型（peers/storage.go 的内部常量）。
// 要让 entities.id 与 Telethon 对得上，只能把取值写在这里；
// 前缀不认识时报错而不是猜，避免往共享的会话库里写进错误的 ID。
const (
	prefixUsers   = "users_"
	prefixChats   = "chats_"
	prefixChannel = "channel_"
)

// gotdSessionFile 是 gotd 的 session.Loader 落盘用的 JSON 结构（session 包里的 jsonData 不导出）。
type gotdSessionFile struct {
	Version int
	Data    session.Data
}

// SessionStore 同时实现 gotd 的 session.Storage（sessions 表）与 peers.Storage（entities 表）。
//
// 一个实例对应一个文件，方法可在多个 goroutine 间并发调用。
type SessionStore struct {
	path string
	db   *sql.DB
}

var (
	_ session.Storage = (*SessionStore)(nil)
	_ peers.Storage   = (*SessionStore)(nil)
)

// OpenSessionStore 打开（不存在则新建）会话库并建好表。
func OpenSessionStore(path string) (*SessionStore, error) {
	// busy_timeout：Python 版可能同时开着同一个库，遇到锁时等一会儿而不是立刻失败。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("打开会话库 %s 失败: %w", path, err)
	}
	// 单连接：CLI 的并发很低，这样能避免多连接之间互相拿到 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &SessionStore{path: path, db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// init 建表并补版本号。
func (s *SessionStore) init() error {
	if _, err := s.db.Exec(sessionSchema); err != nil {
		return fmt.Errorf("初始化会话库 %s 失败: %w", s.path, err)
	}
	// 只在没有版本号时写入：Telethon 升级过的库（比如 8）保持原样不动。
	const stmt = `INSERT INTO version (version) SELECT ?
		WHERE NOT EXISTS (SELECT 1 FROM version)`
	if _, err := s.db.Exec(stmt, telethonVersion); err != nil {
		return fmt.Errorf("写入会话库版本号失败: %w", err)
	}
	return nil
}

// Close 关闭会话库。
func (s *SessionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// LoadSession 实现 gotd 的 session.Storage：从 sessions 表还原会话。
//
// 没有授权（没有行、或 Telethon 写的空 BLOB，见其 _update_session_table）时返回
// session.ErrNotFound，gotd 会按「未登录」处理并走登录流程。
func (s *SessionStore) LoadSession(context.Context) ([]byte, error) {
	var (
		dcID int
		addr sql.NullString
		port sql.NullInt64
		key  []byte
	)
	// 不写 SELECT *：老版 Telethon 的 sessions 是 5 列，新版多了 tmp_auth_key，
	// 按列名取才能两种库都读。
	err := s.db.QueryRow(
		`SELECT dc_id, server_address, port, auth_key FROM sessions LIMIT 1`,
	).Scan(&dcID, &addr, &port, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取会话失败: %w", err)
	}

	// auth key 必须正好 256 字节：长度不对说明库里的不是可用的密钥，
	// 当作未登录，免得 gotd 拿着坏 key 去握手（它会报 "corrupted key"）。
	var authKey crypto.Key
	if len(key) != len(authKey) {
		return nil, session.ErrNotFound
	}
	copy(authKey[:], key)
	authKeyID := authKey.WithID().ID

	data := session.Data{
		DC:        dcID,
		Addr:      joinHostPort(addr.String, port.Int64),
		AuthKey:   authKey[:],
		AuthKeyID: authKeyID[:],
	}
	return json.Marshal(gotdSessionFile{Version: 1, Data: data})
}

// StoreSession 实现 gotd 的 session.Storage：把会话写进 sessions 表。
//
// 与 Telethon 一样只保留一行（它每次都是 delete + insert）。gotd 传进来的是
// session.Loader 序列化好的 JSON，这里解出 DC、地址与 auth key 存下；
// Config 无处可放，丢弃即可——gotd 的 DC 列表与 config 都来自服务端，不依赖落盘的那份。
func (s *SessionStore) StoreSession(_ context.Context, raw []byte) error {
	var file gotdSessionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("解析会话数据失败: %w", err)
	}
	data := file.Data

	host, port := splitHostPort(data.Addr)
	if host == "" {
		// gotd 只在连接成功后落盘，Addr 一直是空的（它从不下发这个字段），
		// 这里按 DC 补一个真实地址，好让 Python 版拿到能用的 server_address。
		host, port = dcAddress(data.DC, nil)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO sessions (dc_id, server_address, port, auth_key, takeout_id) VALUES (?, ?, ?, ?, NULL)`,
		data.DC, host, port, data.AuthKey,
	); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

// Save 实现 peers.Storage，写 Telethon 的 entities 表。
//
// 只更新 hash 与 date，不碰 username / phone / name：这几个字段本工具没有，
// 但可能是 Python 版写进去的，覆盖掉会丢信息。
func (s *SessionStore) Save(_ context.Context, key peers.Key, value peers.Value) error {
	id, err := entityID(key)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO entities (id, hash, date) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET hash = excluded.hash, date = excluded.date`,
		id, value.AccessHash, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("写入 peer 缓存失败: %w", err)
	}
	return nil
}

// Find 实现 peers.Storage。
func (s *SessionStore) Find(_ context.Context, key peers.Key) (peers.Value, bool, error) {
	id, err := entityID(key)
	if err != nil {
		return peers.Value{}, false, err
	}

	var hash int64
	err = s.db.QueryRow(`SELECT hash FROM entities WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return peers.Value{}, false, nil
	}
	if err != nil {
		return peers.Value{}, false, fmt.Errorf("读取 peer 缓存失败: %w", err)
	}
	return peers.Value{AccessHash: hash}, true, nil
}

// SavePhone 实现 peers.Storage：手机号存在 entities.phone 列（Telethon 的 get_entity_rows_by_phone 同款）。
//
// 实体行由 Save 先写好，这里只补号码；MTProto 的手机号是纯数字，
// 交给 SQLite 的 INTEGER 列自行转换（Telethon 也是这么存的）。
func (s *SessionStore) SavePhone(_ context.Context, phone string, key peers.Key) error {
	id, err := entityID(key)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE entities SET phone = ? WHERE id = ?`, phone, id); err != nil {
		return fmt.Errorf("写入手机号失败: %w", err)
	}
	return nil
}

// FindPhone 实现 peers.Storage。手机号只会出现在用户实体上，但这里统一按标记 ID 解回来。
func (s *SessionStore) FindPhone(_ context.Context, phone string) (peers.Key, peers.Value, bool, error) {
	var (
		id   int64
		hash int64
	)
	err := s.db.QueryRow(`SELECT id, hash FROM entities WHERE phone = ?`, phone).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return peers.Key{}, peers.Value{}, false, nil
	}
	if err != nil {
		return peers.Key{}, peers.Value{}, false, fmt.Errorf("按手机号读取 peer 缓存失败: %w", err)
	}

	key, err := peerKey(id)
	if err != nil {
		return peers.Key{}, peers.Value{}, false, err
	}
	return key, peers.Value{AccessHash: hash}, true, nil
}

// GetContactsHash 实现 peers.Storage。
func (s *SessionStore) GetContactsHash(context.Context) (int64, error) {
	var value int64
	err := s.db.QueryRow(`SELECT value FROM store_state WHERE key = ?`, contactsHashKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取联系人版本号失败: %w", err)
	}
	return value, nil
}

// SaveContactsHash 实现 peers.Storage。
func (s *SessionStore) SaveContactsHash(_ context.Context, hash int64) error {
	_, err := s.db.Exec(
		`INSERT INTO store_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		contactsHashKey, hash,
	)
	if err != nil {
		return fmt.Errorf("写入联系人版本号失败: %w", err)
	}
	return nil
}

// ForgetChannel 只删除频道缓存，避免误删同数字 ID 的用户或基础群。
func (s *SessionStore) ForgetChannel(id int64) {
	var channel constant.TDLibPeerID
	channel.Channel(id)
	if _, err := s.db.Exec(`DELETE FROM entities WHERE id = ?`, int64(channel)); err != nil {
		fmt.Fprintf(os.Stderr, "提示：peer 缓存删除失败（不影响下载）: %v\n", err)
	}
}

// ForgetID 删除某个 ID 的所有缓存（不分类型），用于 access_hash 失效后强制重新解析。
//
// ID 只在同类型内唯一，所以把用户、群、频道三种标记形式都删掉——代价可忽略，
// 而不用去猜调用方说的 ID 是哪一类（与之前 JSON 版的行为一致）。
func (s *SessionStore) ForgetID(id int64) {
	var user, chat, channel constant.TDLibPeerID
	user.User(id)
	chat.Chat(id)
	channel.Channel(id)
	if _, err := s.db.Exec(`DELETE FROM entities WHERE id IN (?, ?, ?)`,
		int64(user), int64(chat), int64(channel)); err != nil {
		fmt.Fprintf(os.Stderr, "提示：peer 缓存删除失败（不影响下载）: %v\n", err)
	}
}

// ImportLegacy 把改造前的 <账号标识>.gotd.json 与 <账号标识>.peers.json 合并进本库，
// 返回是否真的导入了（用来提示用户「旧文件已经不用了」）。
//
// 只在库里还没有会话时迁移。已经有会话说明这是新版自己登录出来的状态，
// 此时再读旧文件只会帮倒忙：旧的 auth key 可能已被换掉，旧的 access_hash 也可能
// 比 Python 版刚写进去的旧。文件不存在不算错误——那就是还没登录的正常状态。
// 原文件不删，由用户自行处理。
func (s *SessionStore) ImportLegacy(sessionPath, peersPath string) (bool, error) {
	switch _, err := s.LoadSession(context.Background()); {
	case err == nil:
		return false, nil
	case !errors.Is(err, session.ErrNotFound):
		return false, err
	}

	imported, err := s.importLegacySession(sessionPath)
	if err != nil {
		return imported, err
	}
	if err := s.importLegacyPeers(peersPath); err != nil {
		return imported, err
	}
	return imported, nil
}

// importLegacySession 导入旧版 .gotd.json 里的 auth key 与 DC。
func (s *SessionStore) importLegacySession(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取旧会话文件 %s 失败: %w", path, err)
	}

	var file gotdSessionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		// 旧会话文件里是凭据，不能像缓存那样默默丢掉
		return false, fmt.Errorf("解析旧会话文件 %s 失败: %w", path, err)
	}
	// 旧文件里 gotd 也没写过 Addr，改用文件里那份 DC 列表挑地址最准（比内置列表新）。
	if file.Data.Addr == "" {
		host, port := dcAddress(file.Data.DC, file.Data.Config.DCOptions)
		file.Data.Addr = joinHostPort(host, port)
	}
	migrated, err := json.Marshal(gotdSessionFile{Version: 1, Data: file.Data})
	if err != nil {
		return false, fmt.Errorf("转换旧会话失败: %w", err)
	}
	if err := s.StoreSession(context.Background(), migrated); err != nil {
		return false, err
	}
	return true, nil
}

// importLegacyPeers 导入旧版 peers.json 里存的 access_hash、手机号映射与联系人版本号。
func (s *SessionStore) importLegacyPeers(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取旧 peer 缓存 %s 失败: %w", path, err)
	}

	var legacy struct {
		Peers        map[string]int64  `json:"peers"`
		Phones       map[string]string `json:"phones"`
		ContactsHash int64             `json:"contacts_hash"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		// 损坏的缓存没有挽救价值（重新扫一遍会话列表就能重建），不阻断启动
		fmt.Fprintf(os.Stderr, "提示：旧 peer 缓存 %s 无法解析，已忽略: %v\n", path, err)
		return nil
	}

	for key, hash := range legacy.Peers {
		prefix, id := splitStorageKey(key)
		if err := s.Save(context.Background(), peers.Key{Prefix: prefix, ID: id}, peers.Value{AccessHash: hash}); err != nil {
			return fmt.Errorf("导入旧 peer 缓存失败: %w", err)
		}
	}
	for phone, key := range legacy.Phones {
		prefix, id := splitStorageKey(key)
		if err := s.SavePhone(context.Background(), phone, peers.Key{Prefix: prefix, ID: id}); err != nil {
			return fmt.Errorf("导入旧 peer 缓存失败: %w", err)
		}
	}
	if legacy.ContactsHash != 0 {
		if err := s.SaveContactsHash(context.Background(), legacy.ContactsHash); err != nil {
			return err
		}
	}
	return nil
}

// entityID 把 peers.Key 编码成 Telethon 的 entities.id（带标记 peer id）。
//
// 前缀取值来自 gotd（peers/storage.go 的内部常量，只会是用户/群/频道三种）；
// 出现没见过的前缀直接报错：往共享的会话库里写一个含义不明的 ID，
// 会让 Python 版按错误的类型去解析。
func entityID(key peers.Key) (int64, error) {
	var marked constant.TDLibPeerID
	switch key.Prefix {
	case prefixUsers:
		marked.User(key.ID)
	case prefixChats:
		marked.Chat(key.ID)
	case prefixChannel:
		marked.Channel(key.ID)
	default:
		return 0, fmt.Errorf("未知的 peer 前缀 %q（ID %d）", key.Prefix, key.ID)
	}
	return int64(marked), nil
}

// peerKey 是 entityID 的逆运算：从带标记 peer id 还原 peers.Key。
func peerKey(id int64) (peers.Key, error) {
	marked := constant.TDLibPeerID(id)
	switch {
	case marked.IsChannel():
		return peers.Key{Prefix: prefixChannel, ID: marked.ToPlain()}, nil
	case marked.IsChat():
		return peers.Key{Prefix: prefixChats, ID: marked.ToPlain()}, nil
	case marked.IsUser():
		return peers.Key{Prefix: prefixUsers, ID: marked.ToPlain()}, nil
	}
	return peers.Key{}, fmt.Errorf("entities.id %d 不是有效的 peer ID", id)
}

// splitStorageKey 从旧版 peers.json 的 "<prefix><id>" 键拆出前缀与 ID（ID 是尾部连续数字）。
func splitStorageKey(key string) (prefix string, id int64) {
	i := len(key)
	for i > 0 && key[i-1] >= '0' && key[i-1] <= '9' {
		i--
	}
	if i == len(key) {
		// 没有尾部数字：整个 key 都是前缀（例如只写了 "channel_"）
		return key, 0
	}
	parsed, err := strconv.ParseInt(key[i:], 10, 64)
	if err != nil {
		return "", 0
	}
	return key[:i], parsed
}

// dcAddress 取某个 DC 的首选地址：优先用服务端下发的列表（opts），没有就用 gotd 内置的生产列表。
// 排序规则交给 gotd（IPv4 优先、静态地址优先），与它实际拨号时的选择一致。
func dcAddress(dc int, opts []tg.DCOption) (string, int64) {
	candidates := dcs.FindPrimaryDCs(opts, dc, false)
	if len(candidates) == 0 {
		candidates = dcs.FindPrimaryDCs(dcs.Prod().Options, dc, false)
	}
	if len(candidates) == 0 {
		return "", 0
	}
	return candidates[0].IPAddress, int64(candidates[0].Port)
}

// joinHostPort 拼 "主机:端口"，缺任一项时按空串处理。
func joinHostPort(host string, port int64) string {
	if host == "" || port <= 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.FormatInt(port, 10))
}

// splitHostPort 是 joinHostPort 的逆运算；只有主机没有端口（或反过来的脏数据）时返回空主机。
func splitHostPort(addr string) (string, int64) {
	if addr == "" {
		return "", 0
	}
	host, portRaw, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, err := strconv.ParseInt(portRaw, 10, 64)
	if err != nil {
		return "", 0
	}
	return host, port
}
