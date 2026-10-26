package glm

// ww3 authflow 登录编排（glm-ext login.go 的内核重写，仅 form 范式 —— cdp_capture 需
// wild-work/internal/cdp 整包且 Windows 专属，本仓不引入）。协议事实源 ww3/docs/PLUGIN_CLI.md §5.3。
//
// 安全铁律：LoginFlow 严禁写入任何 token —— flow 文件会被 auth.status 原样上报。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MethodForm 唯一支持的登录范式：粘贴 refresh_token。
const MethodForm = "form"

const loginTimeout = 10 * time.Minute // flow 从创建到过期
const flowGC = 24 * time.Hour         // 过期 flow 文件延迟清理

// LoginFlow 是一个登录会话（持久化在 <data>/auths/flows/<id>.json）。
type LoginFlow struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Method    string `json:"method"`
	State     string `json:"state"` // waiting_input|success|failed|expired|cancelled
	Hint      string `json:"hint,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	ErrorMsg  string `json:"error_msg,omitempty"` // 截断到 160 rune，且绝不含 token
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	AuthFile  string `json:"auth_file,omitempty"`
	ExpiresAt int64  `json:"expires_at"`
}

// LoginManager 编排 form 登录（依赖 ValidateRefreshToken 做验活）。
type LoginManager struct{ dir string }

// NewLoginManager 打开 flow 目录 <data>/auths/flows。
func NewLoginManager(dataDir string) *LoginManager {
	return &LoginManager{dir: filepath.Join(dataDir, "auths", "flows")}
}

func newFlowID() string { return randHex(8) }

func (m *LoginManager) flowPath(id string) string {
	return filepath.Join(m.dir, sanitizeName(id)+".json")
}

func (m *LoginManager) saveFlow(f *LoginFlow) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp := m.flowPath(f.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.flowPath(f.ID))
}

func (m *LoginManager) loadFlow(id string) (*LoginFlow, error) {
	b, err := os.ReadFile(m.flowPath(id))
	if err != nil {
		return nil, NewErr(CUsage, "登录会话 %q 不存在", id)
	}
	f := &LoginFlow{}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, NewErr(CUsage, "登录会话文件损坏: %v", err)
	}
	return f, nil
}

// Begin 发起 form 登录：**同步返回 waiting_input**，等待 Finish 提交 refresh_token。
func (m *LoginManager) Begin(method string) (*LoginFlow, error) {
	if method == "" {
		method = MethodForm
	}
	if method != MethodForm {
		return nil, NewErr(CUsage, "不支持的登录方式 %q（本渠道仅支持 %s）", method, MethodForm)
	}
	m.gc() // 顺手清理过期 flow
	f := &LoginFlow{ID: newFlowID(), CreatedAt: time.Now().Unix(), Method: method,
		State:     "waiting_input",
		Hint:      "在浏览器登录 chatglm.cn 后，从 Cookie 里复制 chatglm_refresh_token 并粘贴提交（F12 → Application → Cookies）",
		ExpiresAt: time.Now().Add(loginTimeout).Unix()}
	return f, m.saveFlow(f)
}

// Status 查询登录进度。
func (m *LoginManager) Status(id string) (*LoginFlow, error) {
	f, err := m.loadFlow(id)
	if err != nil {
		return nil, err
	}
	if !terminalState(f.State) && time.Now().Unix() > f.ExpiresAt { // 超时置 expired
		f.State = "expired"
		f.ErrorKind = "expired"
		_ = m.saveFlow(f)
	}
	return f, nil
}

// Finish 提交 refresh_token，验活成功即落盘凭据并置 success。
// values 里找 refresh_token / refreshToken / token / cookie / session 任一键。
func (m *LoginManager) Finish(ctx context.Context, id string, values map[string]string, store *W3Store) (*LoginFlow, error) {
	f, err := m.Status(id)
	if err != nil {
		return nil, err
	}
	if terminalState(f.State) { // 终态原样返回，不重复处理
		return f, nil
	}
	if time.Now().Unix() > f.ExpiresAt {
		f.State, f.ErrorKind = "expired", "expired"
		_ = m.saveFlow(f)
		return f, nil
	}
	rt := ""
	for _, k := range []string{"refresh_token", "refreshToken", "token", "cookie", "session"} {
		if v := strings.TrimSpace(values[k]); v != "" {
			rt = v
			break
		}
	}
	acct, err := ValidateRefreshToken(ctx, rt)
	if err != nil {
		f.State, f.ErrorKind = "failed", errKindOf(err)
		f.ErrorMsg = truncateRunes(err.Error(), 160)
		_ = m.saveFlow(f)
		return f, nil
	}
	if acct.UID == "" { // 无 uid 无法定位凭据文件，拒绝
		f.State, f.ErrorKind, f.ErrorMsg = "failed", "bad_params", "上游未返回用户 uid，无法落盘凭据"
		_ = m.saveFlow(f)
		return f, nil
	}
	auth := &W3Auth{AccessToken: acct.AccessToken, RefreshToken: acct.RefreshToken,
		ExpiresAt: acct.ExpiresAt, UserID: acct.UID, Nickname: acct.Nickname}
	name, err := store.Save(auth, "")
	if err != nil {
		f.State, f.ErrorKind, f.ErrorMsg = "failed", "server", truncateRunes(err.Error(), 160)
		_ = m.saveFlow(f)
		return f, nil
	}
	f.State, f.UID, f.Nickname, f.AuthFile = "success", acct.UID, acct.Nickname, name
	return f, m.saveFlow(f)
}

// Cancel 取消登录。
func (m *LoginManager) Cancel(id string) (*LoginFlow, error) {
	f, err := m.loadFlow(id)
	if err != nil {
		return nil, err
	}
	if !terminalState(f.State) {
		f.State = "cancelled"
		_ = m.saveFlow(f)
	}
	return f, nil
}

// Account 把 flow 转成 authflow.Account 形态（仅 success 终态有意义）。
func (f *LoginFlow) Account() map[string]any {
	return map[string]any{"uid": f.UID, "nickname": f.Nickname, "channel_id": "glm",
		"login_kind": MethodForm, "auth_file": f.AuthFile}
}

func terminalState(s string) bool {
	switch s {
	case "success", "failed", "expired", "cancelled":
		return true
	}
	return false
}

// gc 清理超过 flowGC 的过期 flow 文件（失败静默，主流程不受影响）。
func (m *LoginManager) gc() {
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > flowGC {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}

// truncateRunes 按 rune 截断（错误信息不允许包含 token，调用方负责先脱敏）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// errKindOf 把内核 *Error 映射到 ww3 errkind（IPC_CHAT §4 全集）。非 *Error 一律 server。
func errKindOf(err error) string {
	k, _ := ErrKindAndScope(err)
	return k
}

// ErrKindAndScope 返回 (errkind, scope)。映射表：
//
//	upstream_error→server/channel、usage_error→bad_params/request、auth_error→auth/account、
//	transient_rejection(40012)→soft_rate/account、stream_truncated→truncated/channel、
//	rate_limited→soft_rate/account、not_proven→account_fault/account。
func ErrKindAndScope(err error) (string, string) {
	e, ok := err.(*Error)
	if !ok {
		return "server", "channel"
	}
	switch e.Code {
	case CUsage:
		return "bad_params", "request"
	case CAuth:
		return "auth", "account"
	case CTransient:
		return "soft_rate", "account"
	case CStreamTruncated:
		return "truncated", "channel"
	case CRateLimited:
		return "soft_rate", "account"
	case CNotProven:
		return "account_fault", "account"
	default: // CUpstream
		return "server", "channel"
	}
}
