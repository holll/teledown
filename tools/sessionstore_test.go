package tools

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/peers"
)

var ctx = context.Background()

const testChannelID = 1965235618

// 用与 peers 包一致的 Key 形态（Prefix 取值由 peers 包决定）。
var channelKey = peers.Key{Prefix: "channel_", ID: testChannelID}

// newTestStore 建一个落在临时目录的会话库，测试结束自动关闭。
func newTestStore(t *testing.T) *SessionStore {
	t.Helper()
	s, err := OpenSessionStore(filepath.Join(t.TempDir(), "abc.session"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// reopen 关闭后重新打开同一个文件，模拟「下次进程启动」。
func reopen(t *testing.T, s *SessionStore) *SessionStore {
	t.Helper()
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestSessionStoreSaveFind(t *testing.T) {
	s := newTestStore(t)

	if _, found, err := s.Find(ctx, channelKey); err != nil || found {
		t.Errorf("空存储不应命中: found=%v err=%v", found, err)
	}

	if err := s.Save(ctx, channelKey, peers.Value{AccessHash: 777}); err != nil {
		t.Fatal(err)
	}
	v, found, err := s.Find(ctx, channelKey)
	if err != nil || !found {
		t.Fatalf("保存后应命中: found=%v err=%v", found, err)
	}
	if v.AccessHash != 777 {
		t.Errorf("AccessHash = %d，期望 777", v.AccessHash)
	}

	// 覆盖写入
	if err := s.Save(ctx, channelKey, peers.Value{AccessHash: 888}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Find(ctx, channelKey); v.AccessHash != 888 {
		t.Errorf("覆盖后 AccessHash = %d，期望 888", v.AccessHash)
	}
}

func TestSessionStorePersistsAcrossInstances(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(ctx, channelKey, peers.Value{AccessHash: 12345}); err != nil {
		t.Fatal(err)
	}
	userKey := peers.Key{Prefix: "users_", ID: 42}
	if err := s.Save(ctx, userKey, peers.Value{AccessHash: 999}); err != nil {
		t.Fatal(err)
	}

	// 重新打开：等价于下次进程启动时命中缓存，无需再扫会话列表
	second := reopen(t, s)
	if v, found, _ := second.Find(ctx, channelKey); !found || v.AccessHash != 12345 {
		t.Errorf("重启后频道缓存应命中 12345，得到 %d found=%v", v.AccessHash, found)
	}
	if v, found, _ := second.Find(ctx, userKey); !found || v.AccessHash != 999 {
		t.Errorf("重启后用户缓存应命中 999，得到 %d found=%v", v.AccessHash, found)
	}
}

func TestSessionStorePhoneMapping(t *testing.T) {
	s := newTestStore(t)

	userKey := peers.Key{Prefix: "users_", ID: 42}
	if err := s.Save(ctx, userKey, peers.Value{AccessHash: 555}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePhone(ctx, "8613800138000", userKey); err != nil {
		t.Fatal(err)
	}

	key, value, found, err := s.FindPhone(ctx, "8613800138000")
	if err != nil || !found {
		t.Fatalf("手机号应命中: found=%v err=%v", found, err)
	}
	if key != userKey || value.AccessHash != 555 {
		t.Errorf("手机号映射结果不对: key=%+v value=%+v", key, value)
	}

	// 重启后依然命中
	reloaded := reopen(t, s)
	if _, _, found, _ := reloaded.FindPhone(ctx, "8613800138000"); !found {
		t.Error("手机号映射应持久化")
	}
	if _, _, found, _ := reloaded.FindPhone(ctx, "8613800000000"); found {
		t.Error("未保存的手机号不应命中")
	}
}

func TestSessionStoreContactsHash(t *testing.T) {
	s := newTestStore(t)

	if h, err := s.GetContactsHash(ctx); err != nil || h != 0 {
		t.Errorf("初始 hash 应为 0，得到 %d err=%v", h, err)
	}
	if err := s.SaveContactsHash(ctx, 4242); err != nil {
		t.Fatal(err)
	}
	if h, _ := reopen(t, s).GetContactsHash(ctx); h != 4242 {
		t.Errorf("重启后 hash = %d，期望 4242", h)
	}
}

func TestSessionStoreForgetID(t *testing.T) {
	s := newTestStore(t)
	keys := []peers.Key{
		channelKey,
		{Prefix: prefixUsers, ID: testChannelID},
		{Prefix: prefixChats, ID: testChannelID},
	}
	for _, key := range keys {
		if err := s.Save(ctx, key, peers.Value{AccessHash: 1}); err != nil {
			t.Fatal(err)
		}
	}
	other := peers.Key{Prefix: "channel_", ID: 999}
	if err := s.Save(ctx, other, peers.Value{AccessHash: 2}); err != nil {
		t.Fatal(err)
	}

	s.ForgetID(testChannelID)

	for _, key := range keys {
		if _, found, err := s.Find(ctx, key); err != nil || found {
			t.Errorf("ForgetID 后所有类型都不应命中: key=%+v found=%v err=%v", key, found, err)
		}
	}
	if _, found, _ := s.Find(ctx, other); !found {
		t.Error("不应误删其它 ID 的缓存")
	}
	if _, found, _ := reopen(t, s).Find(ctx, channelKey); found {
		t.Error("ForgetID 应同步落盘")
	}
}

// TestEntityIDTelethonEncoding 盯住 entities.id 的编码：Telethon 用的是带标记 peer id，
// 写错了 Python 版就会按错误的类型去解析（见 tools/sessionstore.go 文件头）。
func TestEntityIDTelethonEncoding(t *testing.T) {
	cases := []struct {
		key  peers.Key
		want int64
	}{
		{peers.Key{Prefix: "channel_", ID: 1965235618}, -1001965235618},
		{peers.Key{Prefix: "chats_", ID: 7}, -7},
		{peers.Key{Prefix: "users_", ID: 42}, 42},
	}
	for _, c := range cases {
		got, err := entityID(c.key)
		if err != nil {
			t.Fatalf("entityID(%+v) 出错: %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("entityID(%+v) = %d，期望 %d", c.key, got, c.want)
		}

		back, err := peerKey(got)
		if err != nil {
			t.Fatalf("peerKey(%d) 出错: %v", got, err)
		}
		if back != c.key {
			t.Errorf("peerKey(%d) = %+v，期望 %+v", got, back, c.key)
		}
	}

	if _, err := entityID(peers.Key{Prefix: "unknown_", ID: 1}); err == nil {
		t.Error("陌生的前缀应当报错，而不是写进含义不明的 ID")
	}
}

// TestSessionStoreSessionRoundTrip 验证 auth key、DC 与地址的往返，
// 并且产出的字节要能被 gotd 自己的 session.Loader 读出来。
func TestSessionStoreSessionRoundTrip(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("空库应当返回 ErrNotFound，得到 %v", err)
	}

	var key crypto.Key
	for i := range key {
		key[i] = byte(i)
	}
	keyID := key.WithID().ID

	raw := mustSessionJSON(t, session.Data{
		DC:        5,
		Addr:      "91.108.56.116:443",
		AuthKey:   key[:],
		AuthKeyID: keyID[:],
		Salt:      183579926430176377,
	})
	if err := s.StoreSession(ctx, raw); err != nil {
		t.Fatal(err)
	}

	// gotd 那边的读法
	loaded, err := (&session.Loader{Storage: s}).Load(ctx)
	if err != nil {
		t.Fatalf("gotd 的 session.Loader 读不出来: %v", err)
	}
	if loaded.DC != 5 {
		t.Errorf("DC = %d，期望 5", loaded.DC)
	}
	if string(loaded.AuthKey) != string(key[:]) {
		t.Error("auth key 不一致")
	}
	if loaded.Addr != "91.108.56.116:443" {
		t.Errorf("Addr = %q，期望 91.108.56.116:443", loaded.Addr)
	}

	// 重新登录（换个 DC）时应只留一行
	if err := s.StoreSession(ctx, mustSessionJSON(t, session.Data{DC: 2, Addr: "149.154.167.41:443", AuthKey: key[:]})); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("sessions 应只留一行，实际 %d 行", rows)
	}
}

// TestSessionStoreSessionDefaultAddress 检查 gotd 没给地址时会按 DC 补一个，
// 否则 Python 版拿到的 server_address 是空的。
func TestSessionStoreSessionDefaultAddress(t *testing.T) {
	s := newTestStore(t)

	var key crypto.Key
	for i := range key {
		key[i] = byte(255 - i)
	}
	if err := s.StoreSession(ctx, mustSessionJSON(t, session.Data{DC: 5, AuthKey: key[:]})); err != nil {
		t.Fatal(err)
	}

	var addr sql.NullString
	var port sql.NullInt64
	if err := s.db.QueryRow(`SELECT server_address, port FROM sessions`).Scan(&addr, &port); err != nil {
		t.Fatal(err)
	}
	if !addr.Valid || addr.String == "" {
		t.Error("server_address 不应为空")
	}
	if !port.Valid || port.Int64 != 443 {
		t.Errorf("port = %v，期望 443", port)
	}
}

// TestSessionStoreTelethonSchema 盯住表结构：与 Telethon 的 SQLiteSession 不一致的话，
// Python 版就读不了这个文件（版本号写 7 的理由见 tools/sessionstore.go 文件头）。
func TestSessionStoreTelethonSchema(t *testing.T) {
	s := newTestStore(t)

	wantTables := map[string][]string{
		"version":      {"version"},
		"sessions":     {"dc_id", "server_address", "port", "auth_key", "takeout_id"},
		"entities":     {"id", "hash", "username", "phone", "name", "date"},
		"sent_files":   {"md5_digest", "file_size", "type", "id", "hash"},
		"update_state": {"id", "pts", "qts", "date", "seq"},
	}
	for table, wantCols := range wantTables {
		rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
		if err != nil {
			t.Fatalf("读取 %s 表结构失败: %v", table, err)
		}
		var cols []string
		for rows.Next() {
			var (
				cid       int
				name      string
				ctype     string
				notNull   int
				dfltValue sql.NullString
				pk        int
			)
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			cols = append(cols, name)
		}
		rows.Close()
		if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
			t.Errorf("%s 表的列 = %v，期望 %v", table, cols, wantCols)
		}
	}

	var version int
	if err := s.db.QueryRow(`SELECT version FROM version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != telethonVersion {
		t.Errorf("version = %d，期望 %d", version, telethonVersion)
	}
}

// TestSessionStoreImportLegacy 用改造前真实文件的结构做迁移验证：
// .gotd.json 里的 auth key / DC / 地址，.peers.json 里的 access_hash。
func TestSessionStoreImportLegacy(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "324e27135634409d.gotd.json")
	peersPath := filepath.Join(dir, "324e27135634409d.peers.json")

	var key crypto.Key
	for i := range key {
		key[i] = byte(i % 251)
	}
	writeFile(t, sessionPath, legacySessionJSON(t, key))
	// 与真实文件一致的内容（channel_1965235618 的值就是它的 access_hash）
	writeFile(t, peersPath, `{"peers":{"channel_1965235618":-121202923386486007}}`)

	s := newTestStore(t)
	imported, err := s.ImportLegacy(sessionPath, peersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		t.Fatal("应报告导入了会话")
	}

	loaded, err := (&session.Loader{Storage: s}).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DC != 5 || string(loaded.AuthKey) != string(key[:]) {
		t.Errorf("导入的会话不对: DC=%d key 一致=%v", loaded.DC, string(loaded.AuthKey) == string(key[:]))
	}
	// 地址取自旧文件里的 DC 列表（91.108.56.116:443），不是 gotd 内置列表里的 91.108.56.191
	if loaded.Addr != "91.108.56.116:443" {
		t.Errorf("Addr = %q，期望 91.108.56.116:443", loaded.Addr)
	}

	if v, found, err := s.Find(ctx, channelKey); err != nil || !found || v.AccessHash != -121202923386486007 {
		t.Errorf("peer 缓存应被导入: found=%v hash=%d err=%v", found, v.AccessHash, err)
	}

	// 再跑一次：不覆盖已有会话，也不覆盖已有的 access_hash
	// （登录状态与 hash 都可能比旧文件新，比如 Python 版刚写过）
	if err := s.StoreSession(ctx, mustSessionJSON(t, session.Data{DC: 2, Addr: "149.154.167.41:443", AuthKey: key[:]})); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, channelKey, peers.Value{AccessHash: 42}); err != nil {
		t.Fatal(err)
	}

	imported, err = s.ImportLegacy(sessionPath, peersPath)
	if err != nil {
		t.Fatal(err)
	}
	if imported {
		t.Error("已有会话时不应再次导入")
	}
	if after, _ := (&session.Loader{Storage: s}).Load(ctx); after.DC != 2 {
		t.Errorf("已有会话被旧文件覆盖了: DC=%d", after.DC)
	}
	if v, _, _ := s.Find(ctx, channelKey); v.AccessHash != 42 {
		t.Errorf("已有 access_hash 被旧文件覆盖了: %d", v.AccessHash)
	}
}

// TestSessionStoreImportLegacyPhonesAndHash 覆盖旧文件里的手机号映射与联系人版本号。
func TestSessionStoreImportLegacyPhonesAndHash(t *testing.T) {
	dir := t.TempDir()
	peersPath := filepath.Join(dir, "abc.peers.json")
	writeFile(t, peersPath, `{
	  "peers": {"users_42": 555},
	  "phones": {"8613800138000": "users_42"},
	  "contacts_hash": 4242
	}`)

	s := newTestStore(t)
	imported, err := s.ImportLegacy(filepath.Join(dir, "missing.gotd.json"), peersPath)
	if err != nil {
		t.Fatal(err)
	}
	if imported {
		t.Error("没有旧会话文件时不应报告导入了会话")
	}

	key, value, found, err := s.FindPhone(ctx, "8613800138000")
	if err != nil || !found {
		t.Fatalf("手机号映射应被导入: found=%v err=%v", found, err)
	}
	if key != (peers.Key{Prefix: "users_", ID: 42}) || value.AccessHash != 555 {
		t.Errorf("手机号映射不对: key=%+v value=%+v", key, value)
	}
	if h, _ := s.GetContactsHash(ctx); h != 4242 {
		t.Errorf("联系人版本号 = %d，期望 4242", h)
	}
}

// TestSessionStoreImportLegacyMissingAndCorrupt 旧文件缺失或损坏都不能挡住启动。
func TestSessionStoreImportLegacyMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	missingSession := filepath.Join(dir, "nope.gotd.json")
	missingPeers := filepath.Join(dir, "nope.peers.json")

	s := newTestStore(t)
	if imported, err := s.ImportLegacy(missingSession, missingPeers); imported || err != nil {
		t.Errorf("文件不存在时按未登录处理: imported=%v err=%v", imported, err)
	}

	corrupt := filepath.Join(dir, "broken.peers.json")
	writeFile(t, corrupt, "{ 这不是合法 JSON")
	if imported, err := s.ImportLegacy(missingSession, corrupt); imported || err != nil {
		t.Errorf("损坏的缓存应被忽略: imported=%v err=%v", imported, err)
	}

	// 损坏的旧会话文件则要报错：里面是凭据，不能默默丢掉
	brokenSession := filepath.Join(dir, "broken.gotd.json")
	writeFile(t, brokenSession, "{ 这不是合法 JSON")
	if _, err := s.ImportLegacy(brokenSession, missingPeers); err == nil {
		t.Error("损坏的旧会话文件应当报错")
	}
}

// TestSessionStoreCorruptFile 会话库损坏时要直接报错：里面还有 auth key，
// 当成「未登录」默默重来一遍，等于让用户白走一次验证码流程。
func TestSessionStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.session")
	writeFile(t, path, "这不是一个 SQLite 文件")

	s, err := OpenSessionStore(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("损坏的会话库应当报错")
	}
}

func TestSplitStorageKey(t *testing.T) {
	cases := []struct {
		key        string
		wantPrefix string
		wantID     int64
	}{
		{"channel_1965235618", "channel_", 1965235618},
		{"users_42", "users_", 42},
		{"chats_7", "chats_", 7},
		{"channel_", "channel_", 0},
	}
	for _, c := range cases {
		prefix, id := splitStorageKey(c.key)
		if prefix != c.wantPrefix || id != c.wantID {
			t.Errorf("splitStorageKey(%q) = (%q, %d)，期望 (%q, %d)",
				c.key, prefix, id, c.wantPrefix, c.wantID)
		}
	}
}

// legacySessionJSON 是按改造前真实 .gotd.json 的结构拼出来的（地址与 DC 5 的选项取自真实文件）。
func legacySessionJSON(t *testing.T, key crypto.Key) string {
	t.Helper()
	keyID := key.WithID().ID
	return fmt.Sprintf(`{
	  "Version": 1,
	  "Data": {
	    "Config": {
	      "ThisDC": 5,
	      "DCOptions": [
	        {"Flags": 1, "Ipv6": true, "ID": 5, "IPAddress": "2001:0b28:f23f:f005:0000:0000:0000:000a", "Port": 443},
	        {"Flags": 16, "Static": true, "ID": 5, "IPAddress": "91.108.56.116", "Port": 443},
	        {"Flags": 0, "ID": 5, "IPAddress": "91.108.56.116", "Port": 443}
	      ]
	    },
	    "DC": 5,
	    "Addr": "",
	    "AuthKey": %q,
	    "AuthKeyID": %q,
	    "Salt": 183579926430176377
	  }
	}`,
		base64.StdEncoding.EncodeToString(key[:]),
		base64.StdEncoding.EncodeToString(keyID[:]))
}

// mustSessionJSON 按 gotd 的落盘格式（session.Loader 的 jsonData）序列化会话数据。
func mustSessionJSON(t *testing.T, data session.Data) []byte {
	t.Helper()
	raw, err := json.Marshal(gotdSessionFile{Version: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
