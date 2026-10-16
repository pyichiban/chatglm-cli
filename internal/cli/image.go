package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyichiban/chatglm-cli/internal/glm"
	"github.com/pyichiban/chatglm-cli/internal/imgutil"
)

// runImage 一键生图（阻塞直到完成或出错）：
//
//	0 张参考图 → chat 通道画图 agent（cogview），一次返回最多 4 图；
//	≥1 张参考图 → 拼版（reference1..N + 红圈，≤500w 像素 JPEG）→ 图床上传 →
//	              image/reference 智能编辑 → 下载产物保存。
//
// 输出契约：item 事件报告各步骤进度，result 给最终载荷，done 收尾（与 chat 同一套事件流）。
func runImage(a *App) error {
	f := a.f
	prompt, refs, err := splitImageArgs(f.Prompt, a.args(), []string(f.Refs))
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return glm.NewErr(glm.CUsage, "提示词为空")
	}
	if f.Ratio != "" && !validRatio(f.Ratio) {
		return glm.NewErr(glm.CUsage, "--ratio %q 不在实测有效集合: 1:1|3:4|4:3|9:16|16:9", f.Ratio)
	}

	if len(refs) == 0 { // ── 文生图：与 chat --agent drawing 同源 ──
		opt := glm.ChatOpt{Agent: glm.ASSISTANT_DRAWING, Prompt: prompt, Model: f.Model,
			Web: f.Web, Ratio: orDefault(f.Ratio, "1:1")}
		if a.dryCall("image", "POST", glm.EP_CHAT, glm.ChatBody(opt)) {
			return nil
		}
		agg, rerr := a.streamChatEvents(opt, "image", true)
		a.ev(&glm.Event{Type: "done", Cmd: "image", ConversationID: agg.ConversationID,
			MessageID: agg.MessageID, Frames: agg.Frames, Finished: agg.Finished,
			Images: agg.Images, ElapsedMS: time.Since(a.start).Milliseconds()})
		if agg.Err != nil {
			return glm.NewErr(glm.CUpstream, "上游生成失败 error_code=%d: %s",
				agg.Err.ErrorCode, agg.Err.ErrMsg)
		}
		if rerr != nil {
			a.stopBestEffort(agg.MessageID)
			return rerr
		}
		saved, err := a.saveAll(agg.Images, f.Out)
		if err != nil {
			return err
		}
		a.result("image.saved", map[string]any{"saved": saved, "count": len(saved)})
		return nil
	}

	// ── 参考图生图：拼版 → 上传 → 智能编辑 → 下载 ──
	for _, p := range refs {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return glm.NewErr(glm.CUsage, "参考图不可读: %s", p)
		}
	}
	if a.dryRun {
		a.result("dry-run", map[string]any{"cmd": "image", "refs": refs,
			"steps": []string{"composite", "upload", "submit", "download"},
			"body":  glm.EditBody("<上传后直链>", prompt, "", f.Ratio)})
		return nil
	}

	// 1) 拼版
	a.step("composite", map[string]any{"refs": len(refs)})
	sheet, w, h, err := imgutil.BuildRefSheet(refs)
	if err != nil {
		return glm.NewErr(glm.CUsage, "拼版失败: %v", err)
	}
	a.step("composite.done", map[string]any{"w": w, "h": h, "pixels": w * h, "bytes": len(sheet)})

	// 2) 上传图床（meitu → icve 回退）
	a.step("upload", nil)
	url, host, err := imgutil.UploadJPEG(sheet)
	if err != nil {
		return glm.NewErr(glm.CUpstream, "参考图上传失败: %v", err)
	}
	a.step("upload.done", map[string]any{"url": url, "host": host})

	// 3) 智能编辑（同步长请求 17~22s）
	raw, err := a.c.EditImage(url, prompt, "", f.Ratio)
	if err != nil {
		return err
	}
	var res struct {
		ImageURL       string `json:"image_url"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ImageURL == "" {
		return glm.NewErr(glm.CUpstream, "image/reference 响应结构异常: %s", truncStr(string(raw), 200))
	}
	a.result("image", map[string]any{"image_url": res.ImageURL, "conversation_id": res.ConversationID})

	// 4) 下载保存
	saved, err := a.saveAll([]string{res.ImageURL}, f.Out)
	if err != nil {
		return err
	}
	a.ev(&glm.Event{Type: "done", Cmd: "image", ConversationID: res.ConversationID,
		Images: []string{res.ImageURL}, Finished: true,
		ElapsedMS: time.Since(a.start).Milliseconds()})
	a.result("image.saved", map[string]any{"saved": saved, "count": len(saved)})
	return nil
}

// splitImageArgs 整理 image 命令的参数：--prompt 优先；否则第一个位置参数是提示词、
// 其余位置参数全是参考图；--ref 与位置参数可混用（--ref 在前）。
func splitImageArgs(promptFlag string, rest []string, refFlags []string) (string, []string, error) {
	refs := append([]string{}, refFlags...)
	if promptFlag != "" {
		return promptFlag, append(refs, rest...), nil
	}
	if len(rest) == 0 {
		return "", nil, glm.NewErr(glm.CUsage,
			"需要提示词：位置参数或 --prompt；参考图跟在提示词后或用 --ref")
	}
	return rest[0], append(refs, rest[1:]...), nil
}

// step 发一条 item 进度事件（data = {"step":...,...}）。
func (a *App) step(step string, extra map[string]any) {
	m := map[string]any{"step": step}
	for k, v := range extra {
		m[k] = v
	}
	a.ev(&glm.Event{Type: "item", Cmd: "image", Data: rawJSON(m)})
}

// saveAll 下载全部图片并保存；返回落盘路径列表。
func (a *App) saveAll(urls []string, out string) ([]string, error) {
	var saved []string
	for i, u := range urls {
		p, err := a.saveImage(u, out, i+1, len(urls))
		if err != nil {
			return saved, err
		}
		saved = append(saved, p)
		a.step("saved", map[string]any{"path": p, "url": u})
	}
	return saved, nil
}

// saveImage 下载一张产物图并按 --out 规则落盘；非 JPEG/PNG 直接报错（可能被 WAF 拦截）。
func (a *App) saveImage(rawurl, out string, idx, total int) (string, error) {
	req, err := http.NewRequestWithContext(a.c.Ctx(), http.MethodGet, rawurl, nil)
	if err != nil {
		return "", glm.NewErr(glm.CUsage, "下载地址无效: %v", err)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return "", glm.NewErr(glm.CUpstream, "下载产物失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", glm.NewErr(glm.CUpstream, "下载产物 HTTP %d: %s", resp.StatusCode, rawurl)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", glm.NewErr(glm.CUpstream, "读产物失败: %v", err)
	}
	if len(data) < 4 || (!bytes.HasPrefix(data, []byte{0xFF, 0xD8}) &&
		!bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'})) {
		return "", glm.NewErr(glm.CUpstream, "产物不是 JPEG/PNG（可能被 WAF 拦截）: %s", truncStr(string(data), 120))
	}
	path := resolveOutPath(out, time.Now().Format("20060102_150405"), idx, total)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", glm.NewErr(glm.CUpstream, "写文件失败: %v", err)
	}
	return path, nil
}

// resolveOutPath 纯函数：out 为空/是目录/无图片扩展名 ⇒ 目录，文件名 base[-idx].jpg；
// out 以 .jpg/.jpeg 结尾 ⇒ 直接用作文件名（多图时在扩展名前插 -idx）。
func resolveOutPath(out, base string, idx, total int) string {
	name := base
	if total > 1 {
		name = fmt.Sprintf("%s-%d", base, idx)
	}
	if out == "" {
		out = "."
	}
	isDir := strings.HasSuffix(out, "/") || strings.HasSuffix(out, "\\")
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		isDir = true
	}
	low := strings.ToLower(out)
	if isDir || (!strings.HasSuffix(low, ".jpg") && !strings.HasSuffix(low, ".jpeg")) {
		return filepath.Join(out, name+".jpg")
	}
	if total > 1 {
		return strings.TrimSuffix(out, filepath.Ext(out)) + fmt.Sprintf("-%d", idx) + filepath.Ext(out)
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
