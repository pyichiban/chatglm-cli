package glm

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestParseHistory 用真实响应（testdata/history.json，2026-10-16 线上取）锁死历史端点的结构。
func TestParseHistory(t *testing.T) {
	res, err := os.ReadFile("testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	turns, err := ParseHistory(res, false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("应有 2 轮，得到 %d", len(turns))
	}
	// 升序：第一轮问「只回复两个字：收到」，第二轮问上一句是什么
	if turns[0].User != "只回复两个字：收到" {
		t.Errorf("turn0 user = %q", turns[0].User)
	}
	if turns[0].Text != "收到" {
		t.Errorf("turn0 text = %q", turns[0].Text)
	}
	if turns[0].MessageID != "6ad16e02581cc70756919adf" {
		t.Errorf("turn0 message_id = %q", turns[0].MessageID)
	}
	if turns[1].Reasoning == "" {
		t.Errorf("turn1 应有 reasoning（思考过程）")
	}
	if !strings.Contains(turns[1].Text, "收到") {
		t.Errorf("turn1 text = %q", turns[1].Text)
	}
	if turns[0].Status != "finish" {
		t.Errorf("status = %q", turns[0].Status)
	}
	if turns[0].Input != nil || turns[0].Output != nil {
		t.Errorf("不带 --raw 时不该塞原始对象")
	}
	// 时间戳是字符串（"2026-10-16 08:21:22"），不是 Unix 秒 ⇒ 原样透出
	if !strings.HasPrefix(turns[0].CreatedAt, "2026-") {
		t.Errorf("created_at = %q", turns[0].CreatedAt)
	}
}

// TestParseHistoryWithRaw 覆盖 --raw：原始 input/output 必须完整保留。
func TestParseHistoryWithRaw(t *testing.T) {
	res, _ := os.ReadFile("testdata/history.json")
	turns, err := ParseHistory(res, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns[0].Input) == 0 || len(turns[0].Output) == 0 {
		t.Errorf("--raw 下 input/output 不能为空")
	}
	var back map[string]any
	if json.Unmarshal(turns[0].Output, &back) != nil {
		t.Errorf("output 不是合法 JSON")
	}
	if _, ok := back["parts"]; !ok {
		t.Errorf("output 应含 parts")
	}
}

// TestParseHistoryEmpty 覆盖 agent 不匹配时上游返回空 messages（CLI 据此提示 --agent）。
func TestParseHistoryEmpty(t *testing.T) {
	turns, err := ParseHistory(json.RawMessage(`{"conversation_id":"x","messages":[]}`), false)
	if err != nil || len(turns) != 0 {
		t.Fatalf("空历史应返回 0 条不报错: %d %v", len(turns), err)
	}
	if _, err := ParseHistory(json.RawMessage(`"nope"`), false); err == nil {
		t.Errorf("结构异常必须报错")
	}
}

// TestHistoryEndpoint 锁死新端点的路径与参数名（实证结果，别退回 page_messages_v2）。
func TestHistoryEndpoint(t *testing.T) {
	if EP_CONV_MESSAGES != "/chatglm/mainchat-api/conversation/messages" {
		t.Errorf("端点被改动: %s", EP_CONV_MESSAGES)
	}
	if EP_HISTORY != "/chatglm/agent-api/conversation/page_messages_v2" {
		t.Errorf("旧端点常量应保留供比对: %s", EP_HISTORY)
	}
}
