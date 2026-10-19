package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type peerTestInvoker func(context.Context, bin.Encoder, bin.Decoder) error

func (f peerTestInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

var _ tg.Invoker = peerTestInvoker(nil)

// 从公开 Resolve 入口走真实 gotd Manager、RPC 分派与临时 SQLite。
func TestResolveExplicitKindDoesNotFallBackToUser(t *testing.T) {
	for _, tc := range []struct {
		name string
		arg  string
		want []string
	}{
		{"chat", "-1965235618", []string{"chat", "dialogs", "chat"}},
		{"channel", "-1001965235618", []string{"channel", "dialogs", "channel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			var calls []string
			api := tg.NewClient(peerTestInvoker(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				switch input.(type) {
				case *tg.MessagesGetChatsRequest:
					calls = append(calls, "chat")
					output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{}
				case *tg.ChannelsGetChannelsRequest:
					calls = append(calls, "channel")
					return tgerr.New(400, "CHANNEL_INVALID")
				case *tg.MessagesGetDialogsRequest:
					calls = append(calls, "dialogs")
					// 同数字用户确实存在，不能把它当成显式指定的群或频道。
					output.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{
						Users: []tg.UserClass{&tg.User{ID: testChannelID, AccessHash: 777}},
					}
				case *tg.UsersGetUsersRequest:
					calls = append(calls, "user")
					output.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.User{ID: testChannelID, AccessHash: 777}}
				default:
					t.Fatalf("unexpected RPC %T", input)
				}
				return nil
			}))
			p, err := NewPeerManager(api, s).Resolve(context.Background(), tc.arg)
			if err == nil || p != nil {
				t.Errorf("explicit %s must fail, got peer=%+v err=%v", tc.name, p, err)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Errorf("RPC calls = %v, want %v", calls, tc.want)
			}
		})
	}
}

func TestResolveChannelFailurePreservesUserHash(t *testing.T) {
	for _, code := range []string{"CHANNEL_INVALID", "CHANNEL_PRIVATE"} {
		t.Run(code, func(t *testing.T) {
			s := newTestStore(t)
			userKey := peers.Key{Prefix: prefixUsers, ID: testChannelID}
			chatKey := peers.Key{Prefix: prefixChats, ID: testChannelID}
			for key, hash := range map[peers.Key]int64{channelKey: 111, userKey: 777, chatKey: 333} {
				if err := s.Save(context.Background(), key, peers.Value{AccessHash: hash}); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			api := tg.NewClient(peerTestInvoker(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				switch req := input.(type) {
				case *tg.ChannelsGetChannelsRequest:
					calls = append(calls, "channel")
					wantHash := int64(111)
					if len(calls) > 1 {
						wantHash = 0 // 第一次失败后已清除频道 hash。
					}
					if ch := req.ID[0].(*tg.InputChannel); ch.ChannelID != testChannelID || ch.AccessHash != wantHash {
						t.Errorf("channel input = %+v, want hash %d", ch, wantHash)
					}
					return tgerr.New(400, code)
				case *tg.MessagesGetChatsRequest:
					calls = append(calls, "chat")
					output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{}
				case *tg.MessagesGetDialogsRequest:
					calls = append(calls, "dialogs")
					// 不重新灌入用户，以免掩盖误删用户 hash。
					output.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{}
				case *tg.UsersGetUsersRequest:
					calls = append(calls, "user")
					u := req.ID[0].(*tg.InputUser)
					if u.UserID != testChannelID || u.AccessHash != 777 {
						t.Errorf("user input = %+v, want original user hash 777", u)
						return errors.New("user hash lost")
					}
					output.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.User{ID: testChannelID, AccessHash: 777}}
				default:
					t.Fatalf("unexpected RPC %T", input)
				}
				return nil
			}))
			p, err := NewPeerManager(api, s).Resolve(context.Background(), "1965235618")
			if err != nil {
				t.Errorf("positive ID should resolve as user: %v", err)
			} else if u, ok := p.Input.(*tg.InputPeerUser); !ok || u.UserID != testChannelID || u.AccessHash != 777 {
				t.Errorf("unexpected peer: %+v", p)
			}
			if want := []string{"channel", "chat", "dialogs", "channel", "user"}; !reflect.DeepEqual(calls, want) {
				t.Errorf("RPC calls = %v, want %v", calls, want)
			}
			// 重开 SQLite 验证实际落盘状态，而不只检查 Manager 的返回值。
			s = reopen(t, s)
			for key, hash := range map[peers.Key]int64{userKey: 777, chatKey: 333} {
				if v, found, err := s.Find(context.Background(), key); err != nil || !found || v.AccessHash != hash {
					t.Errorf("unrelated cache %+v = %+v, found=%v err=%v", key, v, found, err)
				}
			}
			if _, found, err := s.Find(context.Background(), channelKey); err != nil || found {
				t.Errorf("invalid channel cache must be deleted: found=%v err=%v", found, err)
			}
		})
	}
}

// samePeer 是防止翻页卡死（重复请求同一页 → 触发限流）的关键判断。
func TestSamePeer(t *testing.T) {
	cases := []struct {
		name string
		a, b tg.InputPeerClass
		want bool
	}{
		{
			name: "同一个频道（含 access_hash）",
			a:    &tg.InputPeerChannel{ChannelID: 100, AccessHash: 7},
			b:    &tg.InputPeerChannel{ChannelID: 100, AccessHash: 7},
			want: true,
		},
		{
			name: "同 ID 但 access_hash 变了，视为已前进",
			a:    &tg.InputPeerChannel{ChannelID: 100, AccessHash: 7},
			b:    &tg.InputPeerChannel{ChannelID: 100, AccessHash: 8},
			want: false,
		},
		{
			name: "不同频道",
			a:    &tg.InputPeerChannel{ChannelID: 100, AccessHash: 7},
			b:    &tg.InputPeerChannel{ChannelID: 200, AccessHash: 7},
			want: false,
		},
		{
			name: "两个空 peer（起始游标未能推进）",
			a:    &tg.InputPeerEmpty{},
			b:    &tg.InputPeerEmpty{},
			want: true,
		},
		{
			name: "同一个用户",
			a:    &tg.InputPeerUser{UserID: 5, AccessHash: 9},
			b:    &tg.InputPeerUser{UserID: 5, AccessHash: 9},
			want: true,
		},
		{
			name: "同一个普通群",
			a:    &tg.InputPeerChat{ChatID: 3},
			b:    &tg.InputPeerChat{ChatID: 3},
			want: true,
		},
		{
			name: "类型不同",
			a:    &tg.InputPeerChat{ChatID: 3},
			b:    &tg.InputPeerChannel{ChannelID: 3},
			want: false,
		},
		{
			name: "nil 与 nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "nil 与非 nil",
			a:    nil,
			b:    &tg.InputPeerEmpty{},
			want: false,
		},
	}
	for _, c := range cases {
		if got := samePeer(c.a, c.b); got != c.want {
			t.Errorf("%s: samePeer = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// 命中检测要覆盖三种 peer 类型：Telegram 里数字 ID 可能是频道、基础群或用户。
func TestContainsPeerID(t *testing.T) {
	const want int64 = 1965235618
	chats := []tg.ChatClass{
		&tg.Channel{ID: 111},
		&tg.Chat{ID: 222},
		&tg.ChatForbidden{ID: 333},
	}
	users := []tg.UserClass{
		&tg.User{ID: 444},
		&tg.UserEmpty{ID: 555},
	}

	if containsPeerID(chats, users, 999) {
		t.Error("不存在的 ID 不应命中")
	}
	if !containsPeerID(append(chats, &tg.Channel{ID: want}), users, want) {
		t.Error("频道应命中")
	}
	if !containsPeerID(append(chats, &tg.Chat{ID: want}), users, want) {
		t.Error("基础群应命中（-id 也可能是普通群）")
	}
	if !containsPeerID(chats, append(users, &tg.User{ID: want}), want) {
		t.Error("用户应命中")
	}
	// 只有 min 频道时也算命中（是否需要更新由后续解析决定）
	if !containsPeerID([]tg.ChatClass{&tg.Channel{ID: want, Min: true}}, nil, want) {
		t.Error("min 频道也应算命中，避免重复翻页")
	}
}

// 数字 ID 归一与类型判定。
//
// Telegram（TDLib 约定）用算术编码：频道 = -1000000000000 - p、基础群 = -p、用户 = p。
// 这里逐条验证，尤其是 12 位频道 ID —— 用「剥掉 -100 字符串前缀」的老写法在这类 ID 上
// 会算出差 1e12 的结果。
func TestParsePeerID(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantID   int64
		wantKind peerKind
	}{
		{"不带前缀的正数（默认按频道尝试）", "1965235618", 1965235618, kindUnknown},
		{"频道全形式", "-1001965235618", 1965235618, kindChannel},
		{"频道全形式（频道 ID 较短，含前导零）", "-1000000012345", 12345, kindChannel},
		{"频道 ID 达上限（旧写法在此算错）", "-1997852516352", 997852516352, kindChannel},
		{"基础群（负号 + 群 ID）", "-1965235618", 1965235618, kindChat},
		{"基础群（另一个 ID）", "-1234567890", 1234567890, kindChat},
	}
	for _, c := range cases {
		gotID, gotKind, err := parsePeerID(c.in)
		if err != nil {
			t.Errorf("%s: parsePeerID(%q) 报错: %v", c.name, c.in, err)
			continue
		}
		if gotID != c.wantID {
			t.Errorf("%s: parsePeerID(%q) ID = %d，期望 %d", c.name, c.in, gotID, c.wantID)
		}
		if gotKind != c.wantKind {
			t.Errorf("%s: parsePeerID(%q) 类型 = %v，期望 %v", c.name, c.in, gotKind, c.wantKind)
		}
	}

	t.Run("非法输入报错", func(t *testing.T) {
		for _, in := range []string{"abc", "-1000000000000", "99999999999999999999"} {
			if _, _, err := parsePeerID(in); err == nil {
				t.Errorf("parsePeerID(%q) 应报错", in)
			}
		}
	})
}
