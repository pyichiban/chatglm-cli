package channel

// 控制面方法：models.list / config.schema / accounts.* / auth.* / stats.get。
// 形状以 ww3 protocol 的 authflow / meta 结构为准（glm-ext 同款语义）。

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/pyichiban/chatglm-cli/internal/glm"
	"github.com/pyichiban/chatglm-cli/internal/protocol/authflow"
	"github.com/pyichiban/chatglm-cli/internal/protocol/jsonl"
	"github.com/pyichiban/chatglm-cli/internal/protocol/meta"
	"github.com/pyichiban/chatglm-cli/internal/protocol/stat"
)

// ModelsList 返回静态模型表（上游无公开模型接口，静态表即权威；glm-ext 同款结论）。
func (ch *Channel) ModelsList(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	models := make([]meta.ModelInfo, 0, len(staticModels))
	for _, m := range staticModels {
		models = append(models, meta.ModelInfo{ID: m.ID, Name: m.Name,
			Capabilities: meta.Capabilities2{Reasoning: m.Reasoning}})
	}
	return mustJSON(map[string]any{"models": models, "default": "glm/chatglm:moe_53f"}), nil
}

// ConfigSchema 返回 config.json / --config / channel_config 的字段说明（含约束注释）。
func (ch *Channel) ConfigSchema(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	return mustJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"auth":     map[string]any{"type": "string", "title": "凭据标识", "description": "auths 目录下的凭据文件名（去 .json）"},
			"base_url": map[string]any{"type": "string", "title": "上游基址", "default": "https://chatglm.cn", "description": "一般不改"},
			"timeout":  map[string]any{"type": "integer", "title": "非流式请求超时（秒）", "default": 60},
		},
		"note": "glm 渠道不支持代理（IDE 指纹通道直连）；多轮对话与工具调用不受支持",
	}), nil
}

// AccountsList 列举 auths 目录全部凭据（离线，不碰网络）。
func (ch *Channel) AccountsList(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	entries, bad, err := ch.store.List()
	if err != nil {
		return nil, errf("server", "channel", "读取 auths 失败: %v", err)
	}
	accounts := make([]meta.AccountInfo, 0, len(entries))
	for _, e := range entries {
		status := "ok"
		if e.Auth.RefreshToken == "" {
			status = "session_dead" // 没有根凭据，无法续期
		}
		accounts = append(accounts, meta.AccountInfo{
			UID: e.Auth.UserID, Nickname: e.Auth.Nickname, Status: status,
			LoginKind: "form", ExpiresAt: expText(e.Auth.ExpiresAt),
			ChannelID: ID, Region: ID, Prefixes: []string{ID}, AuthFile: e.Name,
		})
	}
	out := map[string]any{"accounts": accounts}
	if len(bad) > 0 {
		out["bad_files"] = bad
	}
	return mustJSON(out), nil
}

// expText Unix 秒 → RFC3339；0 返回空串（未知）。
func expText(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// AccountsRemove 删除凭据文件。params: {auth_file|uid}。
func (ch *Channel) AccountsRemove(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		AuthFile string `json:"auth_file"`
		UID      string `json:"uid"`
	}
	_ = json.Unmarshal(params, &p)
	name := p.AuthFile
	if name == "" && p.UID != "" {
		e, err := ch.store.FindByUID(p.UID)
		if err != nil {
			return nil, errf("not_found", "account", "未找到 uid=%s 的凭据", p.UID)
		}
		name = e.Name
	}
	if name == "" {
		return nil, errf("bad_params", "request", "需要 auth_file 或 uid")
	}
	if err := ch.store.Delete(name); err != nil {
		return nil, errf("server", "channel", "删除凭据失败: %v", err)
	}
	return mustJSON(map[string]any{"removed": name}), nil
}

// ── authflow（form 范式） ────────────────────────────────

// AuthMethods 本渠道唯一支持粘贴 refresh_token（form）。
func (ch *Channel) AuthMethods(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	methods := []authflow.Method{{
		ID: glm.MethodForm, Type: authflow.TypeForm, Label: "粘贴 refresh_token", Default: true, Region: ID,
		Description: "在浏览器登录 chatglm.cn 后，从 Cookie 复制 chatglm_refresh_token 提交（访客 token 无法使用，须登录账号）",
		Fields: map[string]any{
			"refresh_token": map[string]any{"type": "string", "title": "chatglm_refresh_token", "format": "password", "required": true},
		},
	}}
	return mustJSON(authflow.MethodsResult{Region: ID, Methods: methods,
		Note: "refresh_token 每次刷新都会轮换，插件会自动落盘轮换值"}), nil
}

type flowParams struct {
	FlowID string            `json:"flow_id"`
	Method string            `json:"method"`
	Region string            `json:"region"`
	Values map[string]string `json:"values"`
	// 顶层 refresh_token 是 auth.finish 的简化姿势（core/CLI 直接粘 token 免包 values）
	RefreshToken string `json:"refresh_token"`
}

// AuthBegin 发起 form 登录：同步返回 waiting_input。
func (ch *Channel) AuthBegin(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	f, err := ch.logins.Begin(p.Method)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return mustJSON(authflow.BeginResult{FlowID: f.ID, Type: authflow.TypeForm, Method: f.Method,
		State: f.State, Prompt: f.Hint, ExpiresAt: expText(f.ExpiresAt)}), nil
}

// flowResult 把 LoginFlow 摊平成 StatusResult（错误进 Error 字段，Go error 恒 nil —— 契约惯例）。
func flowResult(f *glm.LoginFlow) json.RawMessage {
	r := authflow.StatusResult{State: authflowState(f.State), Hint: f.Hint, NextPollMS: 0}
	if f.ErrorKind != "" {
		r.Error = &authflow.Error{Kind: f.ErrorKind, Message: f.ErrorMsg}
	}
	if f.State == "success" {
		acct := f.Account()
		r.Account = &authflow.Account{UID: f.UID, Nickname: f.Nickname, ChannelID: acct["channel_id"].(string),
			LoginKind: acct["login_kind"].(string), AuthFile: f.AuthFile}
	}
	return mustJSON(r)
}

// authflowState 内部状态串 → authflow.State 常量。
func authflowState(s string) authflow.State {
	switch s {
	case "waiting_input":
		return authflow.StateWaitingInput
	case "success":
		return authflow.StateSuccess
	case "failed":
		return authflow.StateFailed
	case "expired":
		return authflow.StateExpired
	case "cancelled":
		return authflow.StateCancelled
	}
	return authflow.StatePending
}

// AuthStatus 查询登录进度。
func (ch *Channel) AuthStatus(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	f, err := ch.logins.Status(p.FlowID)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// AuthFinish 提交 refresh_token（values.refresh_token 或顶层 refresh_token）。
func (ch *Channel) AuthFinish(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	if p.Values == nil {
		p.Values = map[string]string{}
	}
	if p.RefreshToken != "" {
		p.Values["refresh_token"] = p.RefreshToken
	}
	f, err := ch.logins.Finish(ctx, p.FlowID, p.Values, ch.store)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// AuthCancel 取消登录。
func (ch *Channel) AuthCancel(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	f, err := ch.logins.Cancel(p.FlowID)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// StatsGet 返回流水汇总。params: {days 缺省 7}。
func (ch *Channel) StatsGet(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Days <= 0 {
		p.Days = 7
	}
	if ch.stat == nil {
		return mustJSON(map[string]any{"days": p.Days, "records": []any{}, "daily": []any{}}), nil
	}
	recs := ch.stat.Query(p.Days)
	sort.Slice(recs, func(i, j int) bool { return recs[i].TS < recs[j].TS })
	return mustJSON(map[string]any{"days": p.Days,
		"summary": stat.Summarize(recs), "daily": ch.stat.DailyRows()}), nil
}

// RecordChat 便利命令（直连内核）的 chat 流水；token 未上报记 -1。
func (ch *Channel) RecordChat(model string, ok bool, err error, start time.Time) {
	kind, _ := glm.ErrKindAndScope(err)
	ch.Record(stat.Record{Kind: stat.KindChat, Account: ch.Auth, Model: model, OK: ok, ErrKind: kind,
		PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1,
		TimeMs: int(time.Since(start).Milliseconds())})
}
