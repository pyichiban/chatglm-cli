package glm

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestChatBodyMatchesCapture 对齐抓包 sid 356 的字段集合（首轮 conversation_id 为空串）。
func TestChatBodyMatchesCapture(t *testing.T) {
	body := ChatBody(ChatOpt{Prompt: "你好", Model: DEFAULT_MODEL, Mode: "thinking",
		Effort: "high", Web: true})
	if body["conversation_id"] != "" || body["chat_type"] != CHAT_TYPE || body["project_id"] != "" {
		t.Errorf("首轮字段错: %v %v %v", body["conversation_id"], body["chat_type"], body["project_id"])
	}
	if body["assistant_id"] != DEFAULT_ASSISTANT {
		t.Errorf("默认 agent 应是主对话")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 必须只有当前这一条，得到 %d", len(msgs))
	}
	b, _ := json.Marshal(body)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	meta, _ := back["meta_data"].(map[string]any)
	for _, k := range []string{"cogview", "is_test", "input_question_type", "channel", "draft_id",
		"chat_mode", "selected_model", "is_networking", "quote_log_id", "platform", "reasoning_effort"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("meta_data 缺字段 %s", k)
		}
	}
	if meta["platform"] != "pc" { // 网页版本来就写 pc，而头部指纹是 win：服务端不校验该一致性 [实证 10-15]
		t.Errorf("platform = %v", meta["platform"])
	}
	cw, _ := meta["cogview"].(map[string]any)
	if _, ok := cw["aspect_ratio"]; ok {
		t.Errorf("普通对话不该带 aspect_ratio（只有生图请求才有）")
	}
	if cw["rm_label_watermark"] != true {
		t.Errorf("cogview.rm_label_watermark 应写死 true（实证 false 产物带角标，用户要求永远去水印）: %v", cw)
	}
}

// TestChatBodyMultiturn 覆盖多轮：唯一差别就是 conversation_id。
func TestChatBodyMultiturn(t *testing.T) {
	cid := "6abbaf3a430b0bf06c7b6b1a"
	body := ChatBody(ChatOpt{Prompt: "第二问", CID: cid})
	if body["conversation_id"] != cid {
		t.Errorf("conversation_id = %v", body["conversation_id"])
	}
}

// TestChatBodyDrawing 覆盖生图：生图 = 对话通道换 agent + cogview 参数（docs/生图协议与实操.md §2.1）。
func TestChatBodyDrawing(t *testing.T) {
	body := ChatBody(ChatOpt{Prompt: "画只猫", Agent: ASSISTANT_DRAWING, Ratio: "3:4",
		Style: "ghibli", Scene: "logo_design"})
	if body["assistant_id"] != ASSISTANT_DRAWING {
		t.Errorf("必须是 AI 画图 agent")
	}
	cw := body["meta_data"].(map[string]any)["cogview"].(map[string]any)
	if cw["aspect_ratio"] != "3:4" || cw["style"] != "ghibli" || cw["scene"] != "logo_design" {
		t.Errorf("cogview = %v", cw)
	}
}

// TestEnvelopeAndCode 覆盖信封错误码 → Code/退出码映射（40102/40103=登录失效，40012=瞬时拒绝）。
func TestEnvelopeAndCode(t *testing.T) {
	c := &Client{}
	for _, tc := range []struct {
		body string
		want Code
	}{
		{`{"status":0,"result":{"a":1}}`, 0},
		{`{"status":40103,"message":"user needs login"}`, CAuth},
		{`{"status":40012,"message":"bad request"}`, CTransient},
		{`{"status":1,"message":"invalid"}`, CUpstream},
		{`<html>waf</html>`, CUpstream},
	} {
		_, err := c.envelope(mkResp(200), []byte(tc.body))
		if tc.want == 0 {
			if err != nil {
				t.Errorf("%s 不该报错: %v", tc.body, err)
			}
			continue
		}
		ge, ok := err.(*Error)
		if !ok || ge.Code != tc.want {
			t.Errorf("%s ⇒ 期望 %v，得到 %v", tc.body, tc.want, err)
		}
	}
	if !CTransient.Retryable() || !CStreamTruncated.Retryable() || CAuth.Retryable() {
		t.Errorf("retryable 标注错")
	}
	if int(CStreamTruncated) != 5 || CStreamTruncated.String() != "stream_truncated" {
		t.Errorf("退出码/名称要与契约表一致")
	}
}

// mkResp 造一个最小响应对象（envelope 只看 StatusCode 与 body）。
func mkResp(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}}
}

// TestCIDPattern 覆盖 id 形状校验（24 位小写 hex）。
func TestCIDPattern(t *testing.T) {
	for _, ok := range []string{"6abbaf3a430b0bf06c7b6b1a", "000000000000000000000000"} {
		if !CIDPattern.MatchString(ok) {
			t.Errorf("%s 应合法", ok)
		}
	}
	for _, bad := range []string{"", "@last", "6ABBaf3a430b0bf06c7b6b1a", "abc", "6abbaf3a430b0bf06c7b6b1"} {
		if CIDPattern.MatchString(bad) {
			t.Errorf("%q 应被拒", bad)
		}
	}
}

func TestAgentsTable(t *testing.T) {
	lst := AGENTS()
	if len(lst) != 5 {
		t.Fatalf("内置 agent 表应有 5 项，得到 %d", len(lst))
	}
	for _, m := range lst {
		if !CIDPattern.MatchString(m["id"]) || m["alias"] == "" {
			t.Errorf("agent 表项异常: %v", m)
		}
	}
	if !slices.Contains([]string{"main", "drawing", "read", "video", "study"}, lst[1]["alias"]) {
		t.Errorf("alias 约定不符: %v", lst[1])
	}
}
