package glm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// ── SSE 帧结构（只解析用得上的字段，其余忽略）──────────────────
// 三条实证规则（PROTOCOL §3.6，勿凭直觉改）：
//  1. 上游**不发** data:[DONE]；正常收尾 = 顶层 status=="finish"。
//  2. parts[].status=="init" → content[].text 是**增量**；=="finish" → 该段**完整全文**。
//     实测 356.body：短回答**只有 finish 帧、没有任何 init 增量** ⇒ 只发 init 会把正文全丢掉。
//  3. 流被截断必须报错，不得伪造收尾帧。
type Content struct {
	Type    string  `json:"type"`
	Text    string  `json:"text"`
	Think   string  `json:"think"`
	Code    string  `json:"code"`    // 生图：上游润色后的实际 prompt
	Content string  `json:"content"` // tool_result / quote_result 的文本
	Image   []Image `json:"image"`
}

type Image struct {
	ImageURL string `json:"image_url"`
}

type Part struct {
	ID      string    `json:"id"`
	LogicID string    `json:"logic_id"`
	Status  string    `json:"status"`
	Model   string    `json:"model"`
	Content []Content `json:"content"`
}

type ErrObj struct {
	ErrorCode int    `json:"error_code"`
	ErrMsg    string `json:"err_msg"`
}

type frame struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	AssistantID    string          `json:"assistant_id"`
	Status         json.RawMessage `json:"status"` // 只有**字符串**才是帧状态；数字 = 裸信封噪声
	Parts          []Part          `json:"parts"`
	Error          *ErrObj         `json:"error"`
	LastError      *ErrObj         `json:"last_error"`
	Meta           struct {
		ConversationID string `json:"conversation_id"`
	} `json:"meta_data"`
}

// 内容类型归类（与 Python sse.py 的 TEXT_TYPES/THINK_TYPES/IMAGE_TYPES 逐一对齐）。
var (
	textTypes  = map[string]bool{"text": true, "code": true, "execution_output": true}
	thinkTypes = map[string]bool{"think": true, "tool_result": true, "quote_result": true}
)

func (f *frame) status() string {
	var s string
	_ = json.Unmarshal(f.Status, &s)
	return s
}

// parseFrame 解一帧。噪声返回 ok=false 直接忽略 —— 生图流末尾会有一行**不带 data: 前缀**的
// 裸 JSON 信封 {"status":0,"message":"success","result":null,"rid":"..."}，其 status 是数字 [实证 10-15]。
func parseFrame(payload []byte) (*frame, bool) {
	p := bytes.TrimSpace(payload)
	if len(p) == 0 || p[0] != '{' {
		return nil, false
	}
	var f frame
	if json.Unmarshal(p, &f) != nil || f.Status == nil {
		return nil, false
	}
	var s string
	if json.Unmarshal(f.Status, &s) != nil {
		return nil, false // status 不是字符串 ⇒ 信封噪声
	}
	return &f, true
}

// ── 聚合器 ────────────────────────────────────────────────
// Agg 消费帧并产出「可安全拼接」的 delta 事件：消费方把 text/reasoning 的 delta 串起来，
// 结果与上游 finish 帧的权威全文一致。
type Agg struct {
	ConversationID string   `json:"-"`
	MessageID      string   `json:"-"` // == 上游 history_id
	AgentID        string   `json:"-"`
	Model          string   `json:"-"`
	Images         []string `json:"-"`
	Frames         int      `json:"-"`
	Finished       bool     `json:"-"`
	Err            *ErrObj  `json:"-"`

	order   []string
	texts   map[string]string
	thinks  map[string]string
	seenImg map[string]bool
}

func NewAgg() *Agg {
	return &Agg{order: nil, texts: map[string]string{}, thinks: map[string]string{},
		seenImg: map[string]bool{}}
}

// Text / Reasoning 按段落出现顺序拼接（段落之间用空行分隔，与 Python 版一致）。
func (a *Agg) Text() string      { return joinParts(a.order, a.texts) }
func (a *Agg) Reasoning() string { return joinParts(a.order, a.thinks) }

func joinParts(order []string, m map[string]string) string {
	var sb strings.Builder
	for _, k := range order {
		if v := m[k]; v != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(v)
		}
	}
	return sb.String()
}

// partKey 段落稳定标识：优先 logic_id（同一逻辑段落跨帧恒定），退回 id，再退回序号。
func partKey(p Part, idx int) string {
	switch {
	case p.LogicID != "":
		return p.LogicID
	case p.ID != "":
		return p.ID
	}
	return "#" + strconv.Itoa(idx)
}

// Feed 消费一帧，返回本帧应实时发出的事件。
func (a *Agg) Feed(f *frame) []*Event {
	a.Frames++
	if a.ConversationID == "" {
		a.ConversationID = firstNonEmpty(f.ConversationID, f.Meta.ConversationID)
	}
	if f.AssistantID != "" {
		a.AgentID = f.AssistantID
	}
	if a.MessageID == "" {
		for _, p := range f.Parts {
			if p.ID != "" {
				a.MessageID = p.ID
				break
			}
		}
		a.MessageID = firstNonEmpty(a.MessageID, f.ID)
	}
	switch f.status() {
	case "error":
		if a.Err == nil {
			a.Err = firstErr(f.Error, f.LastError)
		}
	case "finish":
		a.Finished = true
	}
	var out []*Event
	for i, p := range f.Parts {
		if p.Model != "" {
			a.Model = p.Model
		}
		var txt, think string
		for _, c := range p.Content {
			switch {
			case textTypes[c.Type]:
				txt += firstNonEmpty(c.Text, c.Code)
			case thinkTypes[c.Type]:
				think += firstNonEmpty(c.Think, c.Content)
			case c.Type == "image":
				out = append(out, a.imagesEv(c, partKey(p, i))...)
			}
		}
		if think != "" {
			out = append(out, a.merge("reasoning", partKey(p, i), a.thinks, think, p.Status == "finish")...)
		}
		if txt != "" {
			out = append(out, a.merge("text", partKey(p, i), a.texts, txt, p.Status == "finish")...)
		}
	}
	return out
}

// imagesEv 产出生图事件；URL 去重，末帧 one_to_more_finish 的 image:[{}] 空对象自然被跳过。
func (a *Agg) imagesEv(c Content, key string) []*Event {
	var out []*Event
	for _, im := range c.Image {
		if im.ImageURL == "" || a.seenImg[im.ImageURL] {
			continue
		}
		a.seenImg[im.ImageURL] = true
		a.Images = append(a.Images, im.ImageURL)
		out = append(out, &Event{V: EV, Type: "image", Part: key, URL: im.ImageURL,
			PolishedPrompt: c.Code, Index: len(a.Images)})
	}
	return out
}

func firstErr(a, b *ErrObj) *ErrObj {
	if a != nil {
		return a
	}
	if b != nil {
		return b
	}
	return &ErrObj{ErrMsg: "上游返回 error 但没有 error 对象"}
}

// merge 把一个段落的正文并入聚合状态，返回要发的事件。
// 非 finish（增量）⇒ 累加并原样发出；finish（权威全文）⇒ 只补发「还没发出去的后缀」。
func (a *Agg) merge(kind, key string, m map[string]string, content string, whole bool) []*Event {
	a.trackKey(key)
	if !whole {
		m[key] += content
		return []*Event{{V: EV, Type: kind, Part: key, Delta: content}}
	}
	acc := m[key]
	if strings.HasPrefix(content, acc) {
		m[key] = content
		d := content[len(acc):]
		if d == "" {
			return nil // 收尾帧重复推送已发内容 ⇒ 不再发
		}
		return []*Event{{V: EV, Type: kind, Part: key, Delta: d}}
	}
	// 增量与权威全文前缀对不上（丢帧/重排）⇒ 不发 delta，改发 warn 并带全文，信息不丢。
	m[key] = content
	return []*Event{{V: EV, Type: "warn", Code: "delta_resync", Part: key, Text: content,
		Message: kind + " 段落增量与权威全文不一致，请改用本事件的 text"}}
}

func (a *Agg) trackKey(key string) {
	for _, k := range a.order {
		if k == key {
			return
		}
	}
	a.order = append(a.order, key)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ── 读流 ──────────────────────────────────────────────────
// ReadStream 消费 SSE 响应体：逐帧解析、实时回调 emit，返回聚合结果。
// 读到 EOF 但既没见过 finish 也没见过 error ⇒ 返回 CStreamTruncated（绝不补结束帧）。
// 调用方负责关闭 resp.Body。
func ReadStream(r io.Reader, emit func(*Event), hook func(*Agg)) (*Agg, error) {
	agg := NewAgg()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20) // 单帧可能很大（长回答 / 生图 prompt）
	var buf []string                           // 多行 data: 用 \n 拼接（SSE 规范）
	dispatch := func() {
		if len(buf) == 0 {
			return
		}
		payload := []byte(strings.Join(buf, "\n"))
		buf = buf[:0]
		f, ok := parseFrame(payload)
		if !ok {
			return
		}
		evs := agg.Feed(f)
		if hook != nil {
			hook(agg) // 让 CLI 在第一个内容事件之前先把 meta（含懒创建的 conversation_id）发出去
		}
		if emit != nil {
			for _, ev := range evs {
				emit(ev)
			}
		}
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line[5:], " ")
			buf = append(buf, v)
		}
		// event:/id:/retry: 与裸 JSON 行忽略
	}
	dispatch()
	if err := sc.Err(); err != nil {
		return agg, &Error{Code: CStreamTruncated, Message: "SSE 读取中断: " + err.Error()}
	}
	if !agg.Finished && agg.Err == nil {
		return agg, &Error{Code: CStreamTruncated, Message: "共收 " + strconv.Itoa(agg.Frames) +
			" 帧，未收到 finish/error ⇒ 回答不完整，可安全重试"}
	}
	return agg, nil
}
