package glm

// ww3 authstore 适配层：把 ww3 PLUGIN_CLI §5.2 的凭据文件契约（<data>/auths/<name>.json，
// 嵌套形 {"auth":{accessToken,refreshToken,expiresAt,...},"account":{uid,...}}）
// 桥接到本仓内核的 Credential（device_id/user_id/extra_cookies 是 glm 私有必需字段）。
//
// 兼容性：读兼容三种历史写法 —— ① ww3 嵌套形；② glm2cli 旧扁平形 JSON；③ 整条 cookie / 裸 token
// （后两种走 parseCredBytes）。写一律 ww3 嵌套形，glm 私有字段挂进 auth 节点。
// expires_at 单位 Unix 秒 [实证 PLUGIN_CLI §5.2]。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// W3Auth 是 ww3 authstore 单个凭据文件的内存形态。
//
// 形状：{"auth":{...本结构体...},"account":{uid,nickname}}；
// accessToken/refreshToken/expiresAt 是契约钦定键，其余为 glm 私有扩展。
type W3Auth struct {
	AccessToken  string            `json:"accessToken"`
	RefreshToken string            `json:"refreshToken"`
	ExpiresAt    int64             `json:"expiresAt"` // Unix 秒；0=未知
	DeviceID     string            `json:"deviceId,omitempty"`
	UserID       string            `json:"userId,omitempty"`
	ExtraCookies map[string]string `json:"extraCookies,omitempty"`
	Nickname     string            `json:"nickname,omitempty"` // glm 私有：冗余存一份免网络查询

	unknown map[string]json.RawMessage // 契约未知键保留
}

// W3Account 是登录验证成功后的账号快照（也是 authflow 回传形状）。
type W3Account struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // Unix 秒
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
}

// W3Entry 是 List() 的行：凭据标识 + 凭据内容。
type W3Entry struct {
	Name string // 文件名去 .json，即 core 侧 account_uid
	Auth *W3Auth
}

// ── authstore ─────────────────────────────────────────────

// W3Store 管理 <data>/auths/ 目录（ww3 凭据契约）。
type W3Store struct{ Dir string }

// NewW3Store 打开 <data>/auths（目录懒创建）。
func NewW3Store(dataDir string) *W3Store { return &W3Store{Dir: filepath.Join(dataDir, "auths")} }

// sanitizeName 只留文件名安全字符（ww3 惯例）。
func sanitizeName(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// AuthFileName 缺省名：glm-<uid 前 12 位>（region 单一，无需再带前缀）。
func AuthFileName(uid string) string {
	uid = strings.TrimPrefix(strings.TrimSpace(uid), "glm-")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	if uid == "" {
		uid = time.Now().UTC().Format("20060102150405")
	}
	return "glm-" + uid
}

// Save 原子写（tmp+rename 0600）。name 为空时按 uid 推导；返回实际写入的名字。
func (s *W3Store) Save(a *W3Auth, name string) (string, error) {
	if a.RefreshToken == "" && a.AccessToken == "" {
		return "", NewErr(CUsage, "拒绝保存空凭据")
	}
	name = sanitizeName(name)
	if name == "" {
		name = AuthFileName(a.UserID)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	body, err := a.marshalAuth()
	if err != nil {
		return "", err
	}
	// account 节点冗余 uid/nickname，方便 core/人工只看外层
	doc, _ := json.MarshalIndent(map[string]any{
		"auth":    json.RawMessage(body),
		"account": map[string]string{"uid": a.UserID, "nickname": a.Nickname},
	}, "", "  ")
	p := filepath.Join(s.Dir, name+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return name, nil
}

// Load 按名读取凭据；读兼容 ww3 嵌套形 / 旧扁平形 / cookie / 裸 token。
func (s *W3Store) Load(name string) (*W3Auth, error) {
	name = sanitizeName(name)
	if name == "" {
		return nil, NewErr(CUsage, "凭据标识为空")
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, name+".json"))
	if err != nil {
		return nil, NewErr(CAuth, "凭据 %q 不存在或不可读: %v", name, err)
	}
	return ParseW3Auth(b)
}

// ParseW3Auth 解析三种历史写法（嵌套形优先；扁平形复用内核 parseCredBytes）。
func ParseW3Auth(b []byte) (*W3Auth, error) {
	var probe struct {
		Auth json.RawMessage `json:"auth"`
	}
	if err := json.Unmarshal(b, &probe); err == nil && len(probe.Auth) > 0 {
		a := &W3Auth{}
		if err := json.Unmarshal(probe.Auth, a); err != nil {
			return nil, NewErr(CAuth, "凭据 auth 节点解析失败: %v", err)
		}
		if a.AccessToken == "" && a.RefreshToken == "" {
			return nil, NewErr(CAuth, "凭据缺少 accessToken/refreshToken")
		}
		// account 节点补 uid/nickname（auth 节点未必冗余存）
		var top struct {
			Account struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if json.Unmarshal(b, &top) == nil {
			if a.UserID == "" {
				a.UserID = top.Account.UID
			}
			if a.Nickname == "" {
				a.Nickname = top.Account.Nickname
			}
		}
		// 未知键保留（轮换重写时原样带回，绝不丢字段）
		var m map[string]json.RawMessage
		if json.Unmarshal(probe.Auth, &m) == nil {
			for _, k := range []string{"accessToken", "refreshToken", "expiresAt", "deviceId", "userId", "extraCookies", "nickname"} {
				delete(m, k)
			}
			a.unknown = m
		}
		return a, nil
	}
	// 扁平形：glm2cli 旧 JSON / 整条 cookie / 裸 token
	c, err := parseCredBytes(b)
	if err != nil {
		return nil, err
	}
	return AuthFromCredential(c), nil
}

// AuthFromCredential 内核凭据 → ww3 形态（expiresAt 缺失时从 access JWT 补）。
func AuthFromCredential(c *Credential) *W3Auth {
	a := &W3Auth{AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		DeviceID: c.DeviceID, UserID: c.UserID, ExtraCookies: c.ExtraCookies, Nickname: c.nickname}
	if a.ExpiresAt == 0 { // 内核凭据没有显式 expiresAt，用 JWT exp
		a.ExpiresAt = c.AccessExp()
	}
	return a
}

// Credential 转回内核凭据；source 记来源路径，saveFn 接管轮换后的嵌套落盘。
// saveCtx 需要闭包捕获 (store, name)——这里返回未绑定的凭据，绑定在 NewW3Credential。
func (a *W3Auth) Credential(source string) *Credential {
	c := MakeCredential(a.RefreshToken, a.AccessToken, a.DeviceID, a.UserID, "")
	c.Source = source
	c.nickname = a.Nickname
	if a.ExpiresAt > 0 {
		c.w3ExpiresAt = a.ExpiresAt
	}
	return c
}

// AccessExpired 与内核同口径（提前 AUTH_SKEW）；显式 expiresAt 优先于 JWT。
func (a *W3Auth) AccessExpired() bool {
	if a.ExpiresAt > 0 {
		return a.ExpiresAt-time.Now().Unix() < AUTH_SKEW
	}
	return a.Credential("").AccessExpired()
}

// List 全量列举（按名排序；坏文件跳过并返回警告行，不中断主流程）。
func (s *W3Store) List() (entries []W3Entry, bad []string, err error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		a, err := s.Load(name)
		if err != nil {
			bad = append(bad, name)
			continue
		}
		entries = append(entries, W3Entry{Name: name, Auth: a})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, bad, nil
}

// Delete 删除凭据文件。
func (s *W3Store) Delete(name string) error {
	name = sanitizeName(name)
	if name == "" {
		return NewErr(CUsage, "凭据标识为空")
	}
	if err := os.Remove(filepath.Join(s.Dir, name+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// FindByUID 按 uid 找凭据（core 可能给 uid 而非文件名）。
func (s *W3Store) FindByUID(uid string) (*W3Entry, error) {
	entries, _, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Auth.UserID == uid {
			return &entries[i], nil
		}
	}
	return nil, NewErr(CAuth, "未找到 uid=%s 的凭据", uid)
}

// NewW3Credential 构造一个「落盘到 authstore」的内核凭据：
// 刷新轮换 refresh_token 后 Save() 会以 ww3 嵌套形写回原文件（含乐观比对）。
func NewW3Credential(s *W3Store, name string, a *W3Auth) *Credential {
	c := a.Credential(filepath.Join(s.Dir, sanitizeName(name)+".json"))
	c.origin = a.RefreshToken // 乐观比对基准：本次加载时的 refresh
	c.saveFn = func(cur *Credential) error {
		// 乐观比对：磁盘 refresh 已被其它进程轮换 ⇒ 放弃覆盖
		if disk, err := s.Load(name); err == nil && disk.RefreshToken != "" &&
			cur.origin != "" && disk.RefreshToken != cur.origin {
			return fmt.Errorf("凭据 %s 已被其它进程轮换（refresh_token 与本次刷新依据不一致），放弃写回", name)
		}
		// 就地更新加载时的原对象：unknown 未知键原样保留（绝不丢字段），
		// token/device/uid/nickname 换成最新值；expiresAt 跟当前 access JWT 走。
		a.AccessToken, a.RefreshToken = cur.AccessToken, cur.RefreshToken
		a.DeviceID, a.UserID, a.ExtraCookies, a.Nickname = cur.DeviceID, cur.UserID, cur.ExtraCookies, cur.nickname
		if e := cur.AccessExp(); e > 0 {
			a.ExpiresAt = e
		} else {
			a.ExpiresAt = cur.w3ExpiresAt
		}
		_, err := s.Save(a, name)
		return err
	}
	return c
}

// ── 登录验证（glm-ext client.Validate 的内核重写） ──────────

// ValidateRefreshToken 用裸 refresh_token 换全套凭据并验证可用性（访客会被内核拒绝）。
func ValidateRefreshToken(ctx context.Context, rt string) (*W3Account, error) {
	rt = normalizeRefreshToken(rt)
	if rt == "" {
		return nil, NewErr(CUsage, "refresh_token 为空")
	}
	c := MakeCredential(rt, "", "", "", "")
	cli, err := NewClient(ctx, c, Opt{})
	if err != nil {
		return nil, err
	}
	if err := cli.EnsureToken(true); err != nil { // 强制刷新：验证 + 拿 AccessToken/uid
		return nil, err
	}
	acct := &W3Account{AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		ExpiresAt: c.AccessExp(), UID: c.UserID}
	if ui, err := cli.UserInfo(); err == nil { // 昵称尽力而为
		var m map[string]any
		if json.Unmarshal(ui, &m) == nil {
			acct.Nickname, _ = m["nickname"].(string)
		}
	}
	return acct, nil
}

// normalizeRefreshToken 容忍粘贴整条 cookie / 带引号 / 带分号后缀。
func normalizeRefreshToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "chatglm_refresh_token="); i >= 0 {
		s = s[i+len("chatglm_refresh_token="):]
	}
	s = strings.Trim(s, `";‘’'`)
	if i := strings.IndexByte(s, ';'); i >= 0 { // 粘了多条 cookie
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// marshalAuth 序列化 auth 节点：已知键 + 解析时保留的未知键（合并写回）。
func (a *W3Auth) marshalAuth() ([]byte, error) {
	m := map[string]json.RawMessage{}
	for k, v := range a.unknown {
		m[k] = v
	}
	known, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(known, &m); err != nil {
		return nil, err
	}
	return json.MarshalIndent(m, "", "  ")
}
