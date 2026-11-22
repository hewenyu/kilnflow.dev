package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/term"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// login 验证 Server 地址和 admin token，然后保存到 cli.json。
func (a *app) login(ctx context.Context, args []string) error {
	fs := newFlags("login")
	server := fs.String("server", "", "Server 地址")
	token := fs.String("token", "", "admin token")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		// 多余的参数可能是忘了写 --token 的 token，不输出它
		return usageErrorf("多余的参数：token 用 --token 给出，或者从标准输入读取")
	}
	if *server == "" {
		return usageErrorf("需要 --server")
	}
	if *token == "" {
		if *token, err = a.readToken(); err != nil {
			return err
		}
		if *token == "" {
			return usageErrorf("token 为空")
		}
	}
	c := cliConfig{Server: strings.TrimRight(*server, "/"), Token: *token}
	if _, err := newClient(c, loginTimeout).ListRunners(ctx, req(&apiv1.ListRunnersRequest{})); err != nil {
		return fmt.Errorf("用这个 token 访问 %s 失败：%w", c.Server, err)
	}
	if err := saveConfig(a.configPath, c); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已登录 %s（配置保存在 %s）\n", c.Server, a.configPath)
	return nil
}

// readToken 从标准输入读取 token：终端上不回显，否则读一行（便于管道传入）。
func (a *app) readToken() (string, error) {
	if fd, ok := terminalFd(a.stdin); ok {
		fmt.Fprint(a.stderr, "admin token：")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", fmt.Errorf("读取 token：%w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("读取 token：%w", err)
	}
	return strings.TrimSpace(line), nil
}
