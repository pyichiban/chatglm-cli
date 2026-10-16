package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pyichiban/chatglm-cli/internal/glm"
)

// convUsage 同时用于 help 与 schema，避免两处漂移。
const convUsage = `conv 子命令（会话增删改查；「增」由 chat 不带 --cid 时服务端懒创建完成）:
  list                [--page N --size N --all] [--agent main|drawing] [--grep 关键词]
  show       CID      [--scan N]                    元数据 + 上游自动标题
  history    CID      [--agent A] [--raw]           逐条输出 user/text/reasoning/images
  rename     CID TITLE|--title T|--title-file P     改标题
  title      CID                                   只读自动标题
  delete     CID...   --yes [--agent drawing]       批量删（agent 必须与会话匹配，否则静默无效）
  stop       MESSAGE_ID                             中止生成（done.message_id）
  read       MESSAGE_ID                             标记已读`

func runConv(a *App) error {
	args := a.args() // args()[0] 是 conv 子命令
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, convUsage)
		return glm.NewErr(glm.CUsage, "conv 需要子命令")
	}
	sub, rest := args[0], args[1:]
	if a.dryRun { // dry-run 下没有客户端（不碰凭据）：只展开写类子命令的请求体
		switch sub {
		case "rename", "delete", "stop", "read":
		default:
			a.result("dry-run", map[string]any{"cmd": "conv." + sub, "args": rest})
			return nil
		}
	}
	agent, err := resolveAgent(a.f.Agent, "")
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		return convList(a, agent)
	case "show":
		return convShow(a, rest, agent)
	case "history":
		return convHistory(a, rest, agent)
	case "rename":
		return convRename(a, rest)
	case "title":
		return convTitle(a, rest)
	case "delete":
		return convDelete(a, rest, agent)
	case "stop", "read":
		return convCtl(a, sub, rest)
	default:
		fmt.Fprintln(os.Stderr, convUsage)
		return glm.NewErr(glm.CUsage, "未知 conv 子命令: %s", sub)
	}
}

// mustCID 校验位置参数是 24 位小写 hex（conversation_id / history_id 同形状）。
func mustCID(args []string, i int, what string) (string, error) {
	if len(args) <= i {
		return "", glm.NewErr(glm.CUsage, "%s 缺少 %s", what, "ID")
	}
	id := args[i]
	if !glm.CIDPattern.MatchString(id) {
		return "", glm.NewErr(glm.CUsage, "%s 的 ID 必须是 24 位小写 hex，收到 %q", what, id)
	}
	return id, nil
}

// convList 逐页取会话：每条一个 item 事件，末尾 done 带 count。
func convList(a *App, agent string) error {
	const maxPage = 1000 // 防御上限：has_more 异常时不至于死循环
	f := a.f
	page, n := f.Page, 0
	for {
		body, err := a.pageConvList(page, f.Size)
		if err != nil {
			return err
		}
		for _, raw := range body.List {
			if !matchConv(raw, agent, f.Grep) {
				continue
			}
			n++
			a.ev(&glm.Event{Type: "item", Cmd: "conv.list", Data: raw})
		}
		if !f.All || !body.HasMore || page >= maxPage {
			a.ev(&glm.Event{Type: "done", Cmd: "conv.list", Count: n, Frames: page,
				Finished: true, HasMore: body.HasMore})
			return nil
		}
		page++
	}
}

func (a *App) pageConvList(page, size int) (*convPage, error) {
	res, err := a.c.ConvList(page, size)
	if err != nil {
		return nil, err
	}
	var body convPage
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, glm.NewErr(glm.CUpstream, "recent_list 响应结构异常: %v", err)
	}
	return &body, nil
}

type convPage struct {
	HasMore bool              `json:"has_more"`
	List    []json.RawMessage `json:"conversation_list"`
}

func matchConv(raw json.RawMessage, agent, grep string) bool {
	if agent == "" && grep == "" {
		return true
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if agent != "" && fmt.Sprint(m["assistant_id"]) != agent {
		return false
	}
	if grep != "" && !strings.Contains(fmt.Sprint(m["title"]), grep) {
		return false
	}
	return true
}

// convShow 汇总单个会话：recent_list 元数据 + conversation/title（不假设 list 元素字段）。
func convShow(a *App, rest []string, agent string) error {
	cid, err := mustCID(rest, 0, "conv show")
	if err != nil {
		return err
	}
	f := a.f
	size := f.Size
	var meta map[string]any
	for p := 1; p <= f.Scan; p++ {
		body, err := a.pageConvList(p, size)
		if err != nil {
			return err
		}
		for _, raw := range body.List {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && fmt.Sprint(m["conversation_id"]) == cid {
				meta = m
				break
			}
		}
		if meta != nil || !body.HasMore {
			break
		}
	}
	tr, terr := a.c.ConvTitle(cid)
	var title struct {
		Title string `json:"title"`
	}
	if terr == nil {
		_ = json.Unmarshal(tr, &title)
	}
	if meta == nil && terr != nil {
		return terr // 两边都失败才算错
	}
	a.result("conv.show", map[string]any{"conversation_id": cid, "found_in_recent": meta != nil,
		"title": title.Title, "meta": meta, "agent_id": agent})
	return nil
}

// convHistory 取单会话历史 [实证 2026-10-16]：mainchat-api/conversation/messages。
// 默认逐条输出规范化的 item（user/text/reasoning/images），--raw 输出上游原样。
func convHistory(a *App, rest []string, agent string) error {
	cid, err := mustCID(rest, 0, "conv history")
	if err != nil {
		return err
	}
	res, err := a.c.History(cid, agent, map[string]string(a.f.Params))
	if err != nil {
		return err
	}
	if a.f.Raw {
		a.ev(&glm.Event{Type: "result", Cmd: "conv.history", ConversationID: cid, Data: res})
		return nil
	}
	turns, err := glm.ParseHistory(res, false)
	if err != nil {
		return err
	}
	if len(turns) == 0 {
		a.ev(&glm.Event{Type: "warn", Code: "history_empty", ConversationID: cid,
			Message: "历史为空。最常见原因是 --agent 不匹配（会话 = (assistant_id, conversation_id) 二元组）"})
	}
	for _, t := range turns {
		b, _ := json.Marshal(t)
		a.ev(&glm.Event{Type: "item", Cmd: "conv.history", ConversationID: cid,
			MessageID: t.MessageID, Data: b})
	}
	a.ev(&glm.Event{Type: "done", Cmd: "conv.history", ConversationID: cid, Count: len(turns),
		Finished: true})
	return nil
}

func convRename(a *App, rest []string) error {
	cid, err := mustCID(rest, 0, "conv rename")
	if err != nil {
		return err
	}
	title := a.f.Title
	if a.f.TitleFile != "" {
		b, err := os.ReadFile(a.f.TitleFile)
		if err != nil {
			return glm.NewErr(glm.CUsage, "读 --title-file 失败: %v", err)
		}
		title = strings.TrimSpace(string(b))
	}
	if title == "" && len(rest) > 1 {
		title = strings.Join(rest[1:], " ")
	}
	if title == "" {
		return glm.NewErr(glm.CUsage, "rename 需要标题：位置参数、--title 或 --title-file")
	}
	if a.dryCall("conv.rename", "POST", glm.EP_MODIFY_TITLE,
		map[string]any{"conversation_id": cid, "title": title}) {
		return nil
	}
	if _, err := a.c.Rename(cid, title); err != nil {
		return err
	}
	a.result("conv.rename", map[string]any{"conversation_id": cid, "title": title, "ok": true})
	return nil
}

func convTitle(a *App, rest []string) error {
	cid, err := mustCID(rest, 0, "conv title")
	if err != nil {
		return err
	}
	res, err := a.c.ConvTitle(cid)
	if err != nil {
		return err
	}
	a.passthrough("conv.title", res)
	return nil
}

func convDelete(a *App, rest []string, agent string) error {
	var cids []string
	for i := range rest { // 全部位置参数都当 CID（未知 flag 已在 parse 阶段拒掉）
		cid, err := mustCID(rest, i, "conv delete")
		if err != nil {
			return err
		}
		cids = append(cids, cid)
	}
	if len(cids) == 0 {
		return glm.NewErr(glm.CUsage, "delete 需要至少一个 CID")
	}
	if !a.f.Yes {
		return glm.NewErr(glm.CUsage, "delete 会真实删除账号数据，必须显式加 --yes（本 CLI 不交互确认）")
	}
	if a.dryCall("conv.delete", "POST", glm.EP_BULK_DELETE,
		map[string]any{"conversation_ids": cids, "assistant_id": agent}) {
		return nil
	}
	if _, err := a.c.BulkDelete(cids, agent); err != nil {
		return err
	}
	a.result("conv.delete", map[string]any{"deleted": cids, "agent_id": agent, "ok": true})
	return nil
}

// convCtl 中止生成 / 标记已读，参数是 SSE 帧里的 history_id（== done.message_id）。
func convCtl(a *App, sub string, rest []string) error {
	mid, err := mustCID(rest, 0, "conv "+sub)
	if err != nil {
		return err
	}
	var (
		res json.RawMessage
	)
	if sub == "stop" {
		if a.dryCall("conv.stop", "POST", glm.EP_STOP_STREAM, map[string]any{"history_id": mid}) {
			return nil
		}
		res, err = a.c.StopStream(mid)
	} else {
		if a.dryCall("conv.read", "POST", glm.EP_UPDATE_STATUS, map[string]any{"history_id": mid}) {
			return nil
		}
		res, err = a.c.UpdateStatus(mid)
	}
	if err != nil {
		return err
	}
	a.result("conv."+sub, map[string]any{"message_id": mid, "ok": true, "raw": res})
	return nil
}
