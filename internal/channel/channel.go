// Package channel 是 glm2cli 的 ww3 渠道插件层：把 ww3 PLUGIN_CLI / IPC_CHAT 契约
// （chat.generate / images.generate + 控制面方法）桥接到 internal/glm 内核。
//
// 分层纪律：本包不做协议发明，所有行为以 ww3/docs/PLUGIN_CLI.md、IPC_CHAT.md、
// PLUGIN_PROTOCOL.md 为准；glm 私有语义（assistant_id/chat_mode 等）只在模型映射处出现。
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pyichiban/chatglm-cli/internal/glm"
	"github.com/pyichiban/chatglm-cli/internal/protocol/jsonl"
	"github.com/pyichiban/chatglm-cli/internal/protocol/stat"
)

// 插件标识（manifest 与模型前缀）。
const (
	ID      = "glm"
	Name    = "智谱清言"
	Version = "2.0.0"
)

// Errf 快捷构造 jsonl.ErrObj。
func errf(kind, scope, format string, a ...any) *jsonl.ErrObj {
	return &jsonl.ErrObj{Kind: kind, Scope: scope, Message: fmt.Sprintf(format, a...)}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// Channel 是一次插件进程会话的根对象：持有数据目录、authstore、登录编排与统计器。
type Channel struct {
	Data    string
	Auth    string        // --auth 凭据标识（缺省自动选择）
	Base    string        // --base-url 覆盖
	Timeout  time.Duration // --timeout 覆盖（非流式请求）
	Debug    bool
	Insecure bool

	store  *glm.W3Store
	logins *glm.LoginManager
	stat   *stat.Store
	cfg    map[string]any // config.json + --config 合并结果
}

// New 打开数据目录（懒建 auths/logs/stat 子目录）并加载 <data>/config.json。
func New(dataDir string) (*Channel, error) {
	ch := &Channel{Data: dataDir, store: glm.NewW3Store(dataDir), logins: glm.NewLoginManager(dataDir)}
	for _, sub := range []string{"auths", "logs", "stat"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	ch.stat = stat.Open(dataDir) // stat.Open 内部自拼 <dir>/stat 子目录
	// config.json 缺失/损坏都容忍：插件应能在空配置下运行
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			ch.cfg = m
		}
	}
	return ch, nil
}

// SetConfig 合并 --config（json 串或 @file）进基础配置（非空键覆盖）。
func (ch *Channel) SetConfig(configJSON string) error {
	if configJSON == "" {
		return nil
	}
	b := []byte(configJSON)
	if len(configJSON) > 1 && configJSON[0] == '@' {
		var err error
		if b, err = os.ReadFile(configJSON[1:]); err != nil {
			return glm.NewErr(glm.CUsage, "读取 --config 文件失败: %v", err)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return glm.NewErr(glm.CUsage, "--config 不是合法 JSON 对象: %v", err)
	}
	if ch.cfg == nil {
		ch.cfg = m
		return nil
	}
	for k, v := range m {
		if s, ok := v.(string); !ok || s != "" { // 空串视为未设置
			ch.cfg[k] = v
		}
	}
	return nil
}

// cfgStr 取配置字符串键。
func (ch *Channel) cfgStr(key string) string {
	if ch.cfg == nil {
		return ""
	}
	s, _ := ch.cfg[key].(string)
	return s
}

// Client 按请求构造内核客户端：uid（params.account_uid）> channel_config.auth > --auth >
// auths 目录里第一个未过期凭据 > 旧版 glm_credential.json / GLM_* 环境变量（单机直用模式）。
func (ch *Channel) Client(ctx context.Context, uid string, channelCfg json.RawMessage) (*glm.Client, error) {
	// channel_config 覆盖（非空键）
	baseURL, timeout, authName := ch.Base, ch.Timeout, ch.Auth
	var extra map[string]any
	if len(channelCfg) > 0 {
		_ = json.Unmarshal(channelCfg, &extra)
	}
	get := func(k string) string { if extra != nil { s, _ := extra[k].(string); return s }; return "" }
	if v := get("base_url"); v != "" {
		baseURL = v
	} else if v := ch.cfgStr("base_url"); v != "" {
		baseURL = v
	}
	if v := get("timeout"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	if v := get("auth"); v != "" {
		authName = v
	} else if v := ch.cfgStr("auth"); v != "" {
		authName = v
	}

	name := uid
	if name == "" {
		name = authName
	}
	var cred *glm.Credential
	switch {
	case name != "": // 指定凭据标识：只认 <data>/auths/<name>.json
		a, err := ch.store.Load(name)
		if err != nil {
			return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录（凭据 %q 不可用：%v）；先用 glm2cli login 或 auth.begin 落盘凭据", name, err)
		}
		cred = glm.NewW3Credential(ch.store, name, a)
	default: // 未指定：auths 里挑第一个未过期的，再回落旧版凭据文件
		entries, _, err := ch.store.List()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.Auth.AccessExpired() {
				cred = glm.NewW3Credential(ch.store, e.Name, e.Auth)
				break
			}
		}
		if cred == nil { // 旧版单文件（兼容 glm2cli 独立用法）
			var err error
			if cred, err = glm.LoadCredential(""); err != nil {
				// 旧路径的错误是 fmt 错（非 *glm.Error），统一升为 auth/account 语义
				return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录：%v", err)
			}
			if cred == nil {
				return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录：auths 目录无凭据且未找到旧版 glm_credential.json")
			}
		}
	}
	return glm.NewClient(ctx, cred, glm.Opt{Base: baseURL, Timeout: timeout, Debug: ch.Debug, Insecure: ch.Insecure})
}

// Record 写一条流水（stat 未初始化时静默跳过）。token 未上报记 -1 由调用方保证。
func (ch *Channel) Record(rec stat.Record) {
	if ch.stat == nil {
		return
	}
	rec.Channel = ID
	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}
	ch.stat.Append(rec)
}

// Logins 暴露登录编排（CLI login 命令直连用；IPC 走 auth.* 方法）。
func (ch *Channel) Logins() *glm.LoginManager { return ch.logins }

// Store 暴露凭据库（CLI accounts/login 直连用）。
func (ch *Channel) Store() *glm.W3Store { return ch.store }
