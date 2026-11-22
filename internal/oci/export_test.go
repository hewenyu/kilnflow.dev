package oci

import "time"

// NewTestClient 返回重试间隔很短的客户端，供测试使用。
func NewTestClient(name Name, username, password string) *Client {
	c := NewClient(name, username, password, nil)
	c.backoff = time.Millisecond
	return c
}
