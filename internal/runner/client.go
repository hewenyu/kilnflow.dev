package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// CAS 对象的上传和下载（预签名地址，PLAN #8.4）：与 Server 的 RPC、认证和重试见 runnerkit。

// maxErrorBody 是 HTTP 错误响应中读取的正文长度。
const maxErrorBody = 1024

// casURL 把 Server 返回的相对地址补全。
func casURL(c *runnerkit.Client, u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return c.Server() + u
}

// casPut 上传文件到预签名地址；sent 不为 nil 时随发送累计字节数（客户端写出，不代表 Server 已确认）。
// 返回 nil 表示 Server 已收到并校验了对象（2xx）。
func casPut(ctx context.Context, c *runnerkit.Client, u, file string, sent func(int64)) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	var body io.Reader = f
	if sent != nil {
		body = &countingReader{r: f, n: sent}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, casURL(c, u), body)
	if err != nil {
		return hideURL(err)
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP().Do(req)
	if err != nil {
		return hideURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("上传失败：HTTP %d %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	return nil
}

// countingReader 在每次读出数据时回调读出的字节数。
type countingReader struct {
	r io.Reader
	n func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n(int64(n))
	return n, err
}

// hideURL 去掉 net/http 错误中的请求地址：预签名地址带有签名，不能出现在日志和结果说明中。
func hideURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s 请求失败：%w", ue.Op, ue.Err)
	}
	return err
}

// casGet 打开预签名地址的下载流；调用方负责关闭。
func casGet(ctx context.Context, c *runnerkit.Client, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, casURL(c, u), nil)
	if err != nil {
		return nil, hideURL(err)
	}
	resp, err := c.HTTP().Do(req)
	if err != nil {
		return nil, hideURL(err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := readErrorBody(resp.Body)
		_ = resp.Body.Close() // 错误响应已经读完，关闭失败无影响
		return nil, fmt.Errorf("下载失败：HTTP %d %s", resp.StatusCode, msg)
	}
	return resp.Body, nil
}

// readErrorBody 读取错误响应开头的一段正文，用于错误信息。
func readErrorBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, maxErrorBody))
	if err != nil && len(b) == 0 {
		return ""
	}
	return strings.TrimSpace(string(b))
}
