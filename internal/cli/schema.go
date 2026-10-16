package cli

import (
	"fmt"
	"os"

	"github.com/pyichiban/chatglm-cli/internal/glm"
)

// eventSpec 与 glm.Event 的 json tag 一一对应；改字段必须同时升 glm.EV。
var eventSpec = map[string]string{
	"meta":      "对话流第一个事件（拿到首帧后才发，故 conversation_id 一定是真实值）：request_id/conversation_id/message_id/agent_id/model/new/cmd/ts",
	"text":      "正文增量：part(段落 logic_id)+delta。把 delta 依次拼接 == 上游权威全文",
	"reasoning": "思考/工具结果增量：同 text",
	"image":     "生图产物：url/part/polished_prompt(上游润色后的实际 prompt)/index。一次通常 4 张",
	"item":      "列表类命令的逐条数据：cmd+data（conv list 的每个会话）",
	"result":    "单发数据命令的载荷：cmd+data（上游 result 原样或本地整理结果）",
	"done":      "收尾统计：conversation_id/message_id/frames/elapsed_ms/finished/images/text_chars/reasoning_chars/count/new",
	"warn":      "非致命提示：code+message（history_list_empty / credential / delta_resync）",
	"error":     "失败：code+message+detail+retryable+http_status+biz_status+request_id",
}

// exitSpec：退出码 = glm.Code 数值，agent 只靠它 + error 事件判状态。
var exitSpec = map[string]int{
	"ok":                  0,
	"upstream_error":      int(glm.CUpstream),
	"usage_error":         int(glm.CUsage),
	"auth_error":          int(glm.CAuth),
	"transient_rejection": int(glm.CTransient),
	"stream_truncated":    int(glm.CStreamTruncated),
	"rate_limited":        int(glm.CRateLimited),
	"not_proven":          int(glm.CNotProven),
}

var retryableCodes = []string{"transient_rejection", "stream_truncated", "rate_limited"}

func runSchema(a *App) error {
	cmds := map[string]string{}
	for _, c := range commands() {
		cmds[c.Name] = c.Usage
	}
	a.result("schema", map[string]any{
		"version":    VERSION,
		"go":         goVersion(),
		"event_ver":  glm.EV,
		"commands":   cmds,
		"conv":       convUsage,
		"events":     eventSpec,
		"exit_codes": exitSpec,
		"retryable":  retryableCodes,
		"output":     "stdout 每行一个 JSON 事件（UTF-8、\\n 结尾、无 ANSI）；日志全部走 stderr；永不交互",
		"flags":      "所有 flag 可写在任意位置（含命令名前后）；未知 flag 直接 exit 2",
		"prompts":    "chat 提示词优先 --prompt/-p，其次位置参数，stdin 非 TTY 时自动读（-p - 强制 stdin）",
		"ids":        "conversation_id(24hex)=续接句柄，必须显式传 --cid；message_id=history_id=done.message_id，用于 conv stop/read；request_id=X-Request-Id 仅日志关联",
		"state":      "唯一落盘的是凭据文件（refresh_token 每次刷新都轮换，必须持久化）；会话状态由调用方持有 cid，CLI 无本地状态",
		"channel":    "仅 IDE(Electron) 指纹通道：不需要 refer__991，不做退避重试（实测该链路稳定）",
		"gaps":       "conv history 的上游 list 结构未实证（实测返空）；生图 style/scene 已失效；无费率/usage 字段；图像上传 chat_upload 被 WAF 拦，未实现",
	})
	return nil
}

func runHelp(a *App) error {
	args := a.args()
	if len(args) > 0 && args[0] == "version" {
		a.result("version", map[string]any{"version": VERSION, "go": goVersion()})
		return nil
	}
	if len(args) > 0 && args[0] == "conv" {
		fmt.Fprintln(os.Stdout, convUsage)
		return nil
	}
	usage(os.Stdout)
	return nil
}
