package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestResolveSenders(t *testing.T) {
	var names []string
	selfCalls := 0
	api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.UsersGetUsersRequest:
			if len(req.ID) != 1 {
				t.Fatalf("自身查询参数异常: %+v", req)
			}
			if _, ok := req.ID[0].(*tg.InputUserSelf); !ok {
				t.Fatalf("数字发送者不应触发用户凭据查询: %T", req.ID[0])
			}
			selfCalls++
			out.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.User{ID: 9, Self: true}}
		case *tg.ContactsResolveUsernameRequest:
			names = append(names, req.Username)
			*out.(*tg.ContactsResolvedPeer) = tg.ContactsResolvedPeer{
				Peer: &tg.PeerUser{UserID: 42}, Users: []tg.UserClass{&tg.User{ID: 42}},
			}
		default:
			t.Fatalf("意外 RPC: %T", in)
		}
		return nil
	}))
	pm := NewPeerManager(api, newTestStore(t))
	filter, err := pm.ResolveSenders(context.Background(), " 1, @alice | 1, -1000000000001 | me ")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"alice"}) || selfCalls != 1 {
		t.Fatalf("解析请求异常: names=%v selfCalls=%d", names, selfCalls)
	}
	for _, tc := range []struct {
		from tg.PeerClass
		want bool
	}{
		{&tg.PeerUser{UserID: 1}, true},
		{&tg.PeerUser{UserID: 42}, true},
		{&tg.PeerUser{UserID: 9}, true},
		{&tg.PeerChannel{ChannelID: 1}, true},
		{&tg.PeerChat{ChatID: 1}, false},
		{&tg.PeerUser{UserID: 2}, false},
		{nil, false},
	} {
		if got := filter.Match(&tg.Message{FromID: tc.from}); got != tc.want {
			t.Errorf("FromID=%+v: Match=%v，期望 %v", tc.from, got, tc.want)
		}
	}
}

func TestResolveSendersEmptyAndInvalid(t *testing.T) {
	for _, refs := range []string{"", "  ", ",||", "0", "-1000000000000", "9223372036854775808"} {
		t.Run(refs, func(t *testing.T) {
			api := tg.NewClient(peerTestInvoker(func(context.Context, bin.Encoder, bin.Decoder) error {
				t.Fatal("空过滤条件或非法数字不应发起 RPC")
				return nil
			}))
			filter, err := NewPeerManager(api, newTestStore(t)).ResolveSenders(context.Background(), refs)
			if refs == "" || refs == "  " {
				if err != nil || !filter.Match(&tg.Message{}) {
					t.Fatalf("空条件应匹配所有消息: %v", err)
				}
			} else if err == nil || filter != nil {
				t.Fatalf("非法条件应报错而非降级为全部下载: filter=%+v err=%v", filter, err)
			}
		})
	}
}

func TestResolveSendersFailureDoesNotDisableFilter(t *testing.T) {
	wantErr := errors.New("解析失败")
	api := tg.NewClient(peerTestInvoker(func(context.Context, bin.Encoder, bin.Decoder) error {
		return wantErr
	}))
	filter, err := NewPeerManager(api, newTestStore(t)).ResolveSenders(context.Background(), "1,@missing")
	if !errors.Is(err, wantErr) || filter != nil {
		t.Fatalf("解析失败必须阻止下载: filter=%+v err=%v", filter, err)
	}
}

func TestSenderFilterMissingFromID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   int64
		msg  tg.Message
		want bool
	}{
		{"收到私聊", 7, tg.Message{PeerID: &tg.PeerUser{UserID: 7}}, true},
		{"发出私聊", 9, tg.Message{PeerID: &tg.PeerUser{UserID: 7}, Out: true}, true},
		{"接收者不是发送者", 7, tg.Message{PeerID: &tg.PeerUser{UserID: 7}, Out: true}, false},
		{"频道发帖", -1000000000007, tg.Message{PeerID: &tg.PeerChannel{ChannelID: 7}, Post: true}, true},
		{"群里未知发送者", -1000000000007, tg.Message{PeerID: &tg.PeerChannel{ChannelID: 7}}, false},
		{"无法推断", 7, tg.Message{}, false},
		{"匿名署名不是用户ID", 7, tg.Message{PeerID: &tg.PeerChannel{ChannelID: 7}, PostAuthor: "alice"}, false},
		{"转发来源不是发送者", 7, tg.Message{FromID: &tg.PeerUser{UserID: 8}, FwdFrom: tg.MessageFwdHeader{FromID: &tg.PeerUser{UserID: 7}}}, false},
		{"显式频道身份优先于Out", -1000000000007, tg.Message{FromID: &tg.PeerChannel{ChannelID: 7}, PeerID: &tg.PeerUser{UserID: 8}, Out: true}, true},
		{"不以本人覆盖频道身份", 9, tg.Message{FromID: &tg.PeerChannel{ChannelID: 7}, PeerID: &tg.PeerUser{UserID: 8}, Out: true}, false},
		{"显式用户优先于Post", 7, tg.Message{FromID: &tg.PeerUser{UserID: 7}, PeerID: &tg.PeerChannel{ChannelID: 8}, Post: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := &SenderFilter{ids: map[int64]struct{}{tc.id: {}}, selfID: 9}
			if got := filter.Match(&tc.msg); got != tc.want {
				t.Fatalf("Match=%v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestResolveSendersUsernameAsChannel(t *testing.T) {
	api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch in.(type) {
		case *tg.ContactsResolveUsernameRequest:
			*out.(*tg.ContactsResolvedPeer) = tg.ContactsResolvedPeer{
				Peer: &tg.PeerChannel{ChannelID: 7}, Chats: []tg.ChatClass{&tg.Channel{ID: 7}},
			}
		case *tg.UsersGetUsersRequest:
			out.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.User{ID: 9, Self: true}}
		default:
			t.Fatalf("意外 RPC: %T", in)
		}
		return nil
	}))
	filter, err := NewPeerManager(api, newTestStore(t)).ResolveSenders(context.Background(), "@channel")
	if err != nil {
		t.Fatal(err)
	}
	if !filter.Match(&tg.Message{FromID: &tg.PeerChannel{ChannelID: 7}}) || filter.Match(&tg.Message{FromID: &tg.PeerUser{UserID: 7}}) {
		t.Fatal("以频道身份发送的消息必须按类型区分，不能误匹配同数字用户")
	}
}
