package glm

import "encoding/json"

// Event 是一条 JSONL 事件。刻意用**单结构体 + omitempty**（字段顺序即输出顺序，
// 比 map 稳定、比多结构体简单）：机器只需一套解析器，未知字段忽略即可（v:1 前向兼容）。
type Event struct {
	V    int    `json:"v"`
	Type string `json:"type"` // meta|text|reasoning|image|item|result|done|error|warn

	// 标识
	RequestID      string `json:"request_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"` // == 上游 history_id，stop/read 用
	AgentID        string `json:"agent_id,omitempty"`
	Model          string `json:"model,omitempty"`
	New            bool   `json:"new,omitempty"` // true = 本次创建了会话（首轮）
	Cmd            string `json:"cmd,omitempty"`
	TS             int64  `json:"ts,omitempty"` // Unix 秒

	// 流式增量
	Part  string `json:"part,omitempty"`
	Delta string `json:"delta,omitempty"`
	Text  string `json:"text,omitempty"` // 仅在 resync 纠正时出现（正常消费方忽略）

	// 生图
	URL            string `json:"url,omitempty"`
	PolishedPrompt string `json:"polished_prompt,omitempty"` // 上游润色后的实际生图 prompt
	Index          int    `json:"index,omitempty"`

	// 数据类命令的载荷
	Data    json.RawMessage `json:"data,omitempty"`
	HasMore bool            `json:"has_more,omitempty"`

	// 收尾统计
	Frames         int      `json:"frames,omitempty"`
	ElapsedMS      int64    `json:"elapsed_ms,omitempty"`
	Finished       bool     `json:"finished,omitempty"`
	Images         []string `json:"images,omitempty"`
	TextChars      int      `json:"text_chars,omitempty"`
	ReasoningChars int      `json:"reasoning_chars,omitempty"`
	Count          int      `json:"count,omitempty"`

	// 错误
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Retryable  *bool  `json:"retryable,omitempty"` // 指针：nil=不出现（非 error 事件）；error 事件恒出现，false 也要表达「不可重试」
	HTTPStatus int    `json:"http_status,omitempty"`
	BizStatus  int    `json:"biz_status,omitempty"`
}

const EV = 1 // 事件模式版本

// 构造器（紧凑写法，避免 CLI 侧到处铺 struct 字面量）。
func NewEv(t string) *Event { return &Event{V: EV, Type: t} }

func (e *Event) Err(err error) *Event {
	ge, ok := err.(*Error)
	if !ok {
		ge = &Error{Code: CUpstream, Message: err.Error()}
	}
	e.Type, e.Code, e.Message = "error", ge.Code.String(), ge.Message
	rb := ge.Code.Retryable()
	e.Detail, e.Retryable, e.HTTPStatus, e.BizStatus = ge.Detail, &rb, ge.HTTPStatus, ge.BizStatus
	if e.RequestID == "" {
		e.RequestID = ge.RequestID
	}
	return e
}
