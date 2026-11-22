package runnerkit

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
)

// doctorAuthTimeout 是 doctor 连接 Server 的超时。
const doctorAuthTimeout = 15 * time.Second

// Enroll 用一次性令牌注册 Runner（name 为空时用主机名），保存身份信息并返回。state 是注册时上报的本机状态。
// 令牌的 Runner 类型不是 h.Prog.Kind 时不保存这个身份（类型由令牌决定）。调用方负责把 server、name 写回配置。
func Enroll(ctx context.Context, h *Home, server, token, name string, state *runnerv1.RunnerState) (*State, error) {
	server = strings.TrimRight(server, "/")
	if name == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("读取主机名：%w", err)
		}
		name, _, _ = strings.Cut(host, ".")
	}
	key, err := h.Key(true)
	if err != nil {
		return nil, err
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("私钥类型不是 ed25519")
	}
	c := NewClient(server, 0, key)
	resp, err := c.rpc.Enroll(ctx, connect.NewRequest(&runnerv1.EnrollRequest{
		Token: token, Name: name, PublicKey: []byte(pub), State: state,
	}))
	if err != nil {
		return nil, fmt.Errorf("注册失败：%w", err)
	}
	// 旧版 Server 不返回类型，按 ci 处理
	if kind := cmp.Or(resp.Msg.Kind, "ci"); kind != h.Prog.Kind {
		return nil, errors.New(h.Prog.WrongKind(kind))
	}
	st := &State{Server: server, RunnerID: resp.Msg.RunnerId, Name: name, Pool: resp.Msg.Pool}
	if err := h.SaveState(st); err != nil {
		return nil, err
	}
	return st, nil
}

// DoctorIdentity 检查注册信息、私钥和与 Server 的连接（两种 Runner 的 doctor 的开头）。common 是配置中的 Server 地址等。
// 返回注册信息（没有注册时为 nil）。
func DoctorIdentity(ctx context.Context, h *Home, common *Common, check func(ok bool, item, detail string)) *State {
	st, err := h.LoadState()
	if err != nil {
		check(false, "注册", err.Error())
		return nil
	}
	check(true, "注册", fmt.Sprintf("%s（id %d，pool %s）", st.Name, st.RunnerID, st.Pool))
	key, err := h.Key(false)
	if err != nil {
		check(false, "私钥", err.Error())
		return st
	}
	server := common.ServerURL(st)
	c := NewClient(server, st.RunnerID, key)
	cctx, cancel := context.WithTimeout(ctx, doctorAuthTimeout)
	defer cancel()
	if _, err := c.Authenticate(cctx); err != nil {
		check(false, "连接 Server", fmt.Sprintf("%s：%v", server, err))
		return st
	}
	check(true, "连接 Server", server)
	return st
}
