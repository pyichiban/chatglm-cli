package glm

import (
	"os"
	"strings"
	"testing"
)

// 帧回放测试：数据是真实抓包/线上响应（testdata/），锁死 SSE 语义，不发网络请求。

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读 testdata 失败: %v", err)
	}
	return string(b)
}

// 收集一次回放产生的事件（type 序列 + 拼接后的正文）。
func replay(t *testing.T, name string) ([]*Event, *Agg, error) {
	t.Helper()
	var evs []*Event
	agg, err := ReadStream(strings.NewReader(readTestdata(t, name)),
		func(e *Event) { evs = append(evs, e) }, nil)
	return evs, agg, err
}

// TestStreamFirstFrames 覆盖「短回答只有 finish 帧、没有 init 增量」这个坑：
// 若只发 init 的 delta，正文会被整体丢掉。
func TestStreamFirstFrames(t *testing.T) {
	evs, agg, err := replay(t, "stream_first.txt")
	if err != nil {
		t.Fatalf("不该判成截断: %v", err)
	}
	if !agg.Finished || agg.Err != nil {
		t.Fatalf("应正常收尾: finished=%v err=%v", agg.Finished, agg.Err)
	}
	if agg.ConversationID != "6abbaf3a430b0bf06c7b6b1a" {
		t.Errorf("首轮 conversation_id 取自首帧，得到 %q", agg.ConversationID)
	}
	if agg.MessageID != "6abbaf3a430b0bf06c7b6b1b" {
		t.Errorf("message_id(history_id) = %q", agg.MessageID)
	}
	// delta 拼接必须等于权威全文
	var joined strings.Builder
	for _, e := range evs {
		if e.Type == "text" {
			joined.WriteString(e.Delta)
		}
	}
	if joined.String() != agg.Text() {
		t.Errorf("delta 拼接 != 聚合全文\n拼接=%q\n全文=%q", joined.String(), agg.Text())
	}
	if !strings.Contains(agg.Text(), "你好") {
		t.Errorf("正文丢失: %q", agg.Text())
	}
	if agg.Reasoning() == "" {
		t.Errorf("think 段应归入 reasoning")
	}
	// 收尾帧会重复推送同一 text 段落（实测），必须被 dedup 掉
	n := 0
	for _, e := range evs {
		if e.Type == "text" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("重复推送被当成增量发出了 %d 次", n)
	}
}

// TestStreamError 覆盖 abort 场景：顶层 status=error + error 对象，不得判成正常收尾。
func TestStreamError(t *testing.T) {
	_, agg, err := replay(t, "stream_error.txt")
	if err != nil {
		t.Fatalf("error 收尾不该算截断: %v", err)
	}
	if agg.Finished {
		t.Errorf("被 abort 的流不应 finished=true")
	}
	if agg.Err == nil || agg.Err.ErrorCode != 10024 {
		t.Fatalf("应解析出 error_code=10024，得到 %+v", agg.Err)
	}
}

// TestStreamDrawing 覆盖生图：4 张图各一帧、polished prompt 在 image 项的 code 字段、
// 末帧 one_to_more_finish 的空对象要跳过、流末尾**不带 data: 前缀的裸 JSON 信封**必须被忽略。
func TestStreamDrawing(t *testing.T) {
	evs, agg, err := replay(t, "stream_drawing.txt")
	if err != nil {
		t.Fatalf("生图流应正常收尾: %v", err)
	}
	if len(agg.Images) != 4 {
		t.Errorf("期望 4 张图，得到 %d: %v", len(agg.Images), agg.Images)
	}
	for _, u := range agg.Images {
		if !strings.HasPrefix(u, "https://sfile.chatglm.cn/") {
			t.Errorf("图链异常: %s", u)
		}
	}
	var imgs []*Event
	for _, e := range evs {
		if e.Type == "image" {
			imgs = append(imgs, e)
		}
	}
	if len(imgs) != 4 {
		t.Fatalf("image 事件数 = %d", len(imgs))
	}
	for i, e := range imgs {
		if e.PolishedPrompt == "" {
			t.Errorf("第 %d 张图缺少润色后的 prompt(code)", i+1)
		}
		if e.Index != i+1 {
			t.Errorf("index 应递增，得到 %d", e.Index)
		}
	}
	if agg.AgentID != ASSISTANT_DRAWING {
		t.Errorf("agent = %q", agg.AgentID)
	}
	// 生图正文只有一帧空文本 ⇒ 不应产出 text 事件
	for _, e := range evs {
		if e.Type == "text" && e.Delta != "" {
			t.Errorf("生图不该有正文增量: %q", e.Delta)
		}
	}
}

// TestStreamTruncated 覆盖截断：末帧仍是 init 且没有 finish ⇒ 报错，绝不伪造收尾（硬规则 3）。
func TestStreamTruncated(t *testing.T) {
	full := readTestdata(t, "stream_error.txt")
	// 取前半段（必然停在 status=init 的帧上）当作被掐断的流
	cut := full[:strings.LastIndex(full[:len(full)/2], "\n\n")]
	_, agg, err := readSome(cut)
	if err == nil {
		t.Fatalf("截断流必须报错")
	}
	ge, ok := err.(*Error)
	if !ok || ge.Code != CStreamTruncated {
		t.Fatalf("错误类型应是 stream_truncated，得到 %v", err)
	}
	if agg.Finished {
		t.Errorf("截断不得标记 finished")
	}
}

func readSome(s string) ([]*Event, *Agg, error) {
	var evs []*Event
	agg, err := ReadStream(strings.NewReader(s), func(e *Event) { evs = append(evs, e) }, nil)
	return evs, agg, err
}

// TestDeltaResync 覆盖「增量与权威全文前缀对不上」：改发 warn，不能静默丢内容。
func TestDeltaResync(t *testing.T) {
	frames := []string{
		`data: {"conversation_id":"6abbaf3a430b0bf06c7b6b1a","parts":[{"id":"6abbaf3a430b0bf06c7b6b1b","logic_id":"L1","status":"init","content":[{"type":"text","text":"前半"}]}],"status":"init"}`,
		``,
		`data: {"conversation_id":"6abbaf3a430b0bf06c7b6b1a","parts":[{"id":"6abbaf3a430b0bf06c7b6b1b","logic_id":"L1","status":"finish","content":[{"type":"text","text":"完全不同"}]}],"status":"finish"}`,
		``,
	}
	evs, agg, err := readSome(strings.Join(frames, "\n"))
	if err != nil {
		t.Fatalf("收尾正常: %v", err)
	}
	var warn *Event
	for _, e := range evs {
		if e.Type == "warn" {
			warn = e
		}
	}
	if warn == nil || warn.Code != "delta_resync" {
		t.Fatalf("应发出 delta_resync warn，得到 %+v", evs)
	}
	if warn.Text != "完全不同" {
		t.Errorf("warn 必须带权威全文，得到 %q", warn.Text)
	}
	if agg.Text() != "完全不同" {
		t.Errorf("聚合结果应取权威全文，得到 %q", agg.Text())
	}
}

// TestParseFrameNoise 覆盖噪声判定：裸信封（status 是数字）、[DONE]、空行、非 JSON。
func TestParseFrameNoise(t *testing.T) {
	for _, s := range []string{`{"status":0,"message":"success","result":null,"rid":"x"}`,
		`[DONE]`, ``, `not json`, ``} {
		if f, ok := parseFrame([]byte(s)); ok {
			t.Errorf("不该被当成帧: %s -> %+v", s, f)
		}
	}
	if _, ok := parseFrame([]byte(`{"status":"init","parts":[]}`)); !ok {
		t.Errorf("顶层 status 是字符串就该认作帧")
	}
}

// TestMultiLineData 覆盖 SSE 规范里的多行 data: 载荷。
func TestMultiLineData(t *testing.T) {
	src := "data: {\"conversation_id\":\"c1\",\n" +
		"data: \"parts\":[{\"id\":\"m1\",\"logic_id\":\"L\",\"status\":\"finish\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}],\n" +
		"data: \"status\":\"finish\"}\n\n"
	evs, agg, err := readSome(src)
	if err != nil {
		t.Fatalf("多行 data 应能解析: %v", err)
	}
	if agg.Text() != "ok" || len(evs) != 1 {
		t.Errorf("得到 %q / %d 事件", agg.Text(), len(evs))
	}
}

// TestAggOrderStableByLogicID 覆盖段落顺序：finish 帧的全文必须覆盖同名段落，不重排。
func TestAggOrderStableByLogicID(t *testing.T) {
	src := strings.Join([]string{
		`data: {"parts":[{"logic_id":"B","status":"init","content":[{"type":"text","text":"乙"}]}],"status":"init"}`, "",
		`data: {"parts":[{"logic_id":"A","status":"init","content":[{"type":"text","text":"甲"}]}],"status":"init"}`, "",
		`data: {"parts":[{"logic_id":"B","status":"finish","content":[{"type":"text","text":"乙"}]}],"status":"finish"}`, "",
	}, "\n")
	_, agg, err := readSome(src)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Text() != "乙\n甲" {
		t.Errorf("段落顺序应按首次出现: %q", agg.Text())
	}
}
