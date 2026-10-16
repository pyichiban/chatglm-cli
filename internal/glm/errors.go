package glm

import "fmt"

// Code 是错误类别，直接映射 CLI 退出码与 JSONL error 事件的 code 字段。
type Code int

const (
	CUpstream        Code = iota + 1 // 上游业务/HTTP 错误、响应非 JSON（可能被 WAF 拦）
	CUsage                           // 用法错误（未知 flag、缺参数、非法取值）
	CAuth                            // 凭据/鉴权失效（401、40102/40103、refresh 被拒、访客账号）
	CTransient                       // 风控瞬时拒绝 40012（本版不自动重试，手动重试即可）
	CStreamTruncated                 // SSE 未收到 finish/error 就断了 ⇒ 回答不完整，可安全重试
	CRateLimited                     // HTTP 429
	CNotProven                       // 调用了协议里未实证的接口（如 page_messages_v2 的 list 结构）
)

func (c Code) String() string {
	switch c {
	case CUpstream:
		return "upstream_error"
	case CUsage:
		return "usage_error"
	case CAuth:
		return "auth_error"
	case CTransient:
		return "transient_rejection"
	case CStreamTruncated:
		return "stream_truncated"
	case CRateLimited:
		return "rate_limited"
	case CNotProven:
		return "not_proven"
	}
	return "unknown"
}

// Retryable 告诉调用方「原样重跑是否有意义」。
func (c Code) Retryable() bool {
	return c == CTransient || c == CStreamTruncated || c == CRateLimited
}

// Error 是唯一的错误类型（不做异常层次，靠 Code 区分）。
type Error struct {
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"` // 原始响应体片段（截断）
	HTTPStatus int    `json:"http_status,omitempty"`
	BizStatus  int    `json:"biz_status,omitempty"` // 信封里的 status
	RequestID  string `json:"request_id,omitempty"`
}

func (e *Error) Error() string {
	if e.BizStatus != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d, status=%d)", e.Code, e.Message, e.HTTPStatus, e.BizStatus)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewErr 构造错误。
func NewErr(code Code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}
