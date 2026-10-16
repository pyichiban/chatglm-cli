package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/pyichiban/chatglm-cli/internal/glm"
)

// runRaw 是逆向逃生门：直接打任意端点（解信封后原样输出 result），用来补实证而不必改代码。
//
//	例：glm2cli raw --method GET --path /chatglm/agent-api/conversation/page_messages_v2 \
//	        --param conversation_id=xxx --param assistant_id=xxx --param page_size=30 --param page=2
//
// 只走签名与 IDE 指纹，不猜任何业务字段：上游给什么就输出什么。
func runRaw(a *App) error {
	f := a.f
	method := strings.ToUpper(f.Method)
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return glm.NewErr(glm.CUsage, "raw 只支持 GET|POST")
	}
	// 路径归一化。两个现实坑：
	//   1) git-bash 的 MSYS 会把 /chatglm/x 换码成 C:/Program Files/Git/chatglm/x ⇒ 取 /chatglm/ 子串；
	//   2) 避开换码可以不写前导斜杠（--path chatglm/user-api/user/info）⇒ 自动补上。
	p := f.Path
	if i := strings.Index(p, "/chatglm/"); i >= 0 {
		p = p[i:]
	} else if !strings.HasPrefix(p, "chatglm/") {
		return glm.NewErr(glm.CUsage, "--path 必须指向 /chatglm/ 下的端点（也可不写前导斜杠），收到 %q", f.Path)
	} else {
		p = "/" + p
	}
	f.Path = p
	var body any
	if f.Body != "" {
		raw := []byte(f.Body)
		if f.Body == "-" { // 从 stdin 读 JSON 体，避开 shell 引号地狱
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
			if err != nil {
				return glm.NewErr(glm.CUsage, "读 stdin 失败: %v", err)
			}
			raw = b
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return glm.NewErr(glm.CUsage, "--body 不是合法 JSON 对象: %v", err)
		}
		body = m
	}
	if a.dryCall("raw", method, f.Path, body) {
		return nil
	}
	res, err := a.c.Do(method, f.Path, map[string]string(f.Params), body)
	if err != nil {
		return err
	}
	a.ev(&glm.Event{Type: "result", Cmd: "raw", Data: res})
	return nil
}
