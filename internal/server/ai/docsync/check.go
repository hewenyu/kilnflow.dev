package docsync

import (
	"context"
	"errors"
	"net/http"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
)

// 显式只读连接验证的固定枚举：状态与错误码是单一定义来源，admin 记录结果、web 展示结果都从这里取。
// 错误码不含远端返回的 message、响应体或底层 URL 错误（它们可能回显请求头与 token）。
const (
	// DocCheckStatusOK 表示文档集可访问、列举完整、抽查的正文读取通过。
	DocCheckStatusOK = "ok"
	// DocCheckStatusEmpty 表示文档集可访问，但当前没有文档（不抽查正文）。
	DocCheckStatusEmpty = "empty"
	// DocCheckStatusFailed 表示验证失败，错误码说明是哪一步。
	DocCheckStatusFailed = "failed"

	DocCheckAuthFailed         = "auth_failed"
	DocCheckForbidden          = "forbidden"
	DocCheckNotFound           = "not_found"
	DocCheckCollectionNotFound = "collection_not_found"
	DocCheckSampleNotFound     = "sample_not_found"
	DocCheckRateLimited        = "rate_limited"
	DocCheckUpstream           = "upstream_error"
	DocCheckInvalidResponse    = "invalid_response"
	DocCheckInvalidBaseURL     = "invalid_base_url"
	DocCheckNetworkError       = "network_error"
	DocCheckTimeout            = "timeout"
	DocCheckInvalidDocument    = "invalid_document"
	DocCheckSampleInvalid      = "sample_invalid"
)

// DocCheckErrorText 返回错误码给人看的说明（固定文案，不含远端内容）。
func DocCheckErrorText(code string) string {
	switch code {
	case DocCheckAuthFailed:
		return "文档系统拒绝了这组凭据：请确认 token 有效、没有过期"
	case DocCheckForbidden:
		return "这组凭据没有访问权限：请确认它是该文档集的成员"
	case DocCheckNotFound:
		return "文档系统返回不存在：请确认站点地址和文档集 id"
	case DocCheckCollectionNotFound:
		return "这组凭据可访问的文档集里没有配置的文档集：请确认文档集 id，或给 token 授予访问权限"
	case DocCheckSampleNotFound:
		return "抽查的文档在列举之后读取不到了：文档可能刚被删除或移走"
	case DocCheckRateLimited:
		return "文档系统限流：请稍后重试"
	case DocCheckUpstream:
		return "文档系统返回内部错误：请稍后重试"
	case DocCheckInvalidResponse:
		return "文档系统返回了无法解析的响应"
	case DocCheckInvalidBaseURL:
		return "文档系统地址无效：必须是 http 或 https 且有主机名"
	case DocCheckNetworkError:
		return "无法连接文档系统：请确认站点地址与网络连通性"
	case DocCheckTimeout:
		return "验证超时：文档系统在时限内没有返回结果"
	case DocCheckInvalidDocument:
		return "文档集里有不满足冻结条件的文档（越出文档集、未发布、已归档或已删除）"
	case DocCheckSampleInvalid:
		return "抽查的文档与列举结果不一致（id、所属文档集或发布状态）"
	}
	return "验证失败"
}

// ClassifyDocProviderError 把文档客户端的错误归入固定枚举：只读取 docprovider.Error 的状态码与白名单错误码，
// 绝不保留或返回远端 message。op 是操作名（collections/documents/document），用于把 404 落到更具体的枚举。
func ClassifyDocProviderError(err error, op string) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DocCheckTimeout
	}
	if errors.Is(err, context.Canceled) {
		return DocCheckTimeout
	}
	// HTTP 成功但响应解析失败（JSON 损坏、data 类型不对）：这是"响应无效"，不是连不上。
	if docprovider.IsDecodeError(err) {
		return DocCheckInvalidResponse
	}
	var pe *docprovider.Error
	if !errors.As(err, &pe) {
		return DocCheckNetworkError
	}
	switch pe.Status {
	case http.StatusUnauthorized:
		return DocCheckAuthFailed
	case http.StatusForbidden:
		return DocCheckForbidden
	case http.StatusNotFound:
		if op == "collections" {
			return DocCheckCollectionNotFound
		}
		if op == "document" {
			return DocCheckSampleNotFound
		}
		return DocCheckNotFound
	case http.StatusTooManyRequests:
		return DocCheckRateLimited
	}
	if pe.Status >= 500 {
		return DocCheckUpstream
	}
	return DocCheckInvalidResponse
}
