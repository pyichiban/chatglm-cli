package glm

import (
	"encoding/json"
	"net/http"
	"regexp"
)

// CIDPattern 是 conversation_id 的形状（24 位小写 hex）[实证 PROTOCOL §3.3]。
var CIDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

// ChatOpt 是一次对话的参数。零值 = 与网页版默认行为一致。
type ChatOpt struct {
	Agent       string // assistant_id，空 = 主对话
	CID         string // conversation_id，空 = 首轮（服务端懒创建会话）
	Prompt      string // 只带当前这一条消息：多轮上下文由服务端按 CID 维护 [实证 PROTOCOL §3.6]
	Model       string
	Mode        string // chat_mode：普通/思考/沉思…，值原样透传
	Effort      string // reasoning_effort（""/thinking/deep_thinking 与 low/high/max 两套写法，见 README）
	Web         bool   // is_networking
	Ratio       string // 生图 aspect_ratio；给了就按生图（cogview）
	Style       string
	Scene       string
	RMWatermark bool // cogview.rm_label_watermark
}

// ChatBody 构造 /assistant/stream 请求体（字段对齐抓包 sid 356/480/781）。
func ChatBody(o ChatOpt) map[string]any {
	agent := o.Agent
	if agent == "" {
		agent = DEFAULT_ASSISTANT
	}
	model := o.Model
	if model == "" {
		model = DEFAULT_MODEL
	}
	cogview := map[string]any{"rm_label_watermark": o.RMWatermark}
	if o.Ratio != "" {
		cogview["aspect_ratio"] = o.Ratio // 非法值上游会静默降级 1:1（docs/生图协议与实操.md §2.3）
	}
	if o.Style != "" {
		cogview["style"] = o.Style // 实测已失效，仅透传
	}
	if o.Scene != "" {
		cogview["scene"] = o.Scene
	}
	meta := map[string]any{
		"cogview":             cogview,
		"is_test":             false,
		"input_question_type": "xxxx",
		"channel":             "",
		"draft_id":            "",
		"chat_mode":           o.Mode,
		"selected_model":      model,
		"is_networking":       o.Web,
		"quote_log_id":        "",
		"platform":            "pc", // ⚠️ 头部指纹是 win，这里网页版本就写 pc；服务端不校验该一致性 [实证 10-15]
	}
	if o.Effort != "" {
		meta["reasoning_effort"] = o.Effort
	}
	return map[string]any{
		"assistant_id":    agent,
		"conversation_id": o.CID,
		"project_id":      "",
		"chat_type":       CHAT_TYPE,
		"meta_data":       meta,
		"messages": []any{map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "text", "text": o.Prompt}}}},
	}
}

// Chat 发起一次对话流：resp 由调用方消费（ReadStream）并关闭。
func (c *Client) Chat(o ChatOpt) (*http.Response, error) {
	return c.Stream(ChatBody(o))
}

// ── 只读类端点 ─────────────────────────────────────────────
func (c *Client) UserInfo() (json.RawMessage, error) {
	return c.Do(http.MethodGet, EP_USER_INFO, nil, nil)
}

func (c *Client) Models() (json.RawMessage, error) {
	return c.Do(http.MethodGet, EP_MODELS, map[string]string{"tag": "available_models"}, nil)
}

// ConvList 返回 {has_more, conversation_list}。翻页是纯页码 [实证 PROTOCOL §3.3]。
func (c *Client) ConvList(page, size int) (json.RawMessage, error) {
	return c.Do(http.MethodPost, EP_CONV_LIST, nil,
		map[string]any{"page": page, "page_size": size})
}

// 单会话历史（含规范化）见 history.go：上游老端点 page_messages_v2 是死路，已改用 mainchat-api/conversation/messages。

func (c *Client) ConvTitle(cid string) (json.RawMessage, error) {
	return c.Do(http.MethodGet, EP_CONV_TITLE, map[string]string{"conversation_id": cid}, nil)
}

// ── 写类端点 ───────────────────────────────────────────────
func (c *Client) Rename(cid, title string) (json.RawMessage, error) {
	return c.Do(http.MethodPost, EP_MODIFY_TITLE, nil,
		map[string]any{"conversation_id": cid, "title": title})
}

// BulkDelete 删会话。单删也走 bulk（无单删端点）。
// ⚠️ agent 必须与会话所属 agent 一致，否则上游**静默无效** [实证]：生图会话要传 ASSISTANT_DRAWING。
func (c *Client) BulkDelete(cids []string, agent string) (json.RawMessage, error) {
	if agent == "" {
		agent = DEFAULT_ASSISTANT
	}
	return c.Do(http.MethodPost, EP_BULK_DELETE, nil,
		map[string]any{"conversation_ids": cids, "assistant_id": agent})
}

// StopStream 中止生成；UpdateStatus 标记已读。两者都用 SSE 帧里的 parts[].id（history_id）。
func (c *Client) StopStream(mid string) (json.RawMessage, error) {
	return c.Do(http.MethodPost, EP_STOP_STREAM, nil, map[string]any{"history_id": mid})
}

func (c *Client) UpdateStatus(mid string) (json.RawMessage, error) {
	return c.Do(http.MethodPost, EP_UPDATE_STATUS, nil, map[string]any{"history_id": mid})
}
