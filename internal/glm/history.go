package glm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// History 拉单会话历史 [实证 2026-10-16]：GET mainchat-api/conversation/messages。
//
// 上游 result 形状：
//
//	{conversation_id, if_plus_model, new_ai_search_conversation, project_id,
//	 messages:[{id, conversation_id, create_time, create_time_str, update_time,
//	            input:{content[…], meta_data{…}}, output:{role,status,parts[…],last_error},
//	            score:{}, memory_score:{}}]}
//
// messages 按 create_time **升序全量**返回；page_size / page 等翻页参数实测被忽略（没有分页）。
// ⚠️ assistant_id 必须与会话匹配（会话 = (assistant_id, conversation_id) 二元组），传错直接空列表。
func (c *Client) History(cid, agent string, extra map[string]string) (json.RawMessage, error) {
	if agent == "" {
		agent = DEFAULT_ASSISTANT
	}
	q := map[string]string{"conversation_id": cid, "assistant_id": agent}
	for k, v := range extra {
		q[k] = v
	}
	return c.Do(http.MethodGet, EP_CONV_MESSAGES, q, nil)
}

// HistoryV2 是老路子 page_messages_v2：**实测始终返回空 list**（PROTOCOL §3.5 记录的那个缺口）。
// 保留只为日后比对，CLI 默认不走它。
func (c *Client) HistoryV2(cid, agent string, size int, extra map[string]string) (json.RawMessage, error) {
	if agent == "" {
		agent = DEFAULT_ASSISTANT
	}
	q := map[string]string{"assistant_id": agent, "conversation_id": cid, "page_size": strconv.Itoa(size)}
	for k, v := range extra {
		q[k] = v
	}
	return c.Do(http.MethodGet, EP_HISTORY, q, nil)
}

// Turn 是一条规范化后的对话记录（一问一答）。Raw 保留上游原样，规范化字段丢了也不影响还原。
type Turn struct {
	MessageID string          `json:"message_id"`
	CreatedAt string          `json:"created_at"`
	User      string          `json:"user"` // 用户输入（文本 + 图片 Markdown）
	Text      string          `json:"text"` // 助手正文（含生图产物的 Markdown）
	Reasoning string          `json:"reasoning,omitempty"`
	Images    []string        `json:"images,omitempty"`
	Status    string          `json:"status,omitempty"` // 助手侧收尾状态（finish/error）
	Input     json.RawMessage `json:"input,omitempty"`  // --raw 时带上上游原始对象
	Output    json.RawMessage `json:"raw,omitempty"`
}

// ParseHistory 把 History 的 result 规范化成 []Turn。
// output 的 parts 结构与 SSE 帧一致 ⇒ 直接复用 Agg 的聚合规则（init 增量 / finish 全文）。
func ParseHistory(res json.RawMessage, withRaw bool) ([]Turn, error) {
	var body struct {
		Messages []struct {
			ID         string          `json:"id"`
			CreateTime string          `json:"create_time"`
			Input      json.RawMessage `json:"input"`
			Output     json.RawMessage `json:"output"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, NewErr(CUpstream, "conversation/messages 响应结构异常: %v", err)
	}
	out := make([]Turn, 0, len(body.Messages))
	for i, m := range body.Messages {
		t := Turn{MessageID: m.ID, CreatedAt: m.CreateTime}
		t.User = renderInput(m.Input)
		var f frame
		if json.Unmarshal(m.Output, &f) == nil {
			agg := NewAgg()
			agg.Feed(&f) // 单帧聚合：历史里的 output 已是终态（status=finish 全文）
			t.Text, t.Reasoning, t.Images, t.Status = agg.Text(), agg.Reasoning(), agg.Images, f.status()
		}
		if t.MessageID == "" {
			t.MessageID = fmt.Sprintf("#%d", i)
		}
		if withRaw {
			t.Input, t.Output = m.Input, m.Output
		}
		out = append(out, t)
	}
	return out, nil
}

// renderInput 把用户侧 input.content 摊成文本：文本原样，图片转 Markdown 链接。
func renderInput(raw json.RawMessage) string {
	var in struct {
		Content []Content `json:"content"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range in.Content {
		switch {
		case textTypes[c.Type]:
			sb.WriteString(firstNonEmpty(c.Text, c.Code))
		case c.Type == "image":
			for _, im := range c.Image {
				if im.ImageURL != "" {
					sb.WriteString(" ![](" + im.ImageURL + ")")
				}
			}
		}
	}
	return sb.String()
}
