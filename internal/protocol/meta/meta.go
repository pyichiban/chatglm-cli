// Package meta 定义插件元数据（manifest）、能力清单与模型/资源信息结构（PLUGIN_PROTOCOL §3/§5）。
package meta

// Manifest 插件元数据（控制面 initialize 上报，也用于清单/商店）。
type Manifest struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Kind         string       `json:"kind"` // channel | hook
	Protocol     ProtocolRng  `json:"protocol"`
	Execution    Execution    `json:"execution"`
	Capabilities Capabilities `json:"capabilities,omitempty"`
	Icon         string       `json:"icon,omitempty"`
	Color        string       `json:"color,omitempty"`
	Description  string       `json:"description,omitempty"`
}

// ProtocolRng 协议版本兼容区间。
type ProtocolRng struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Execution 进程执行模式（PLUGIN_PROTOCOL §3.1，W3-20 定案）。
//
// 两种形态，与 main/protocol/plugin 的 ModeCall / ModeStdio 对应：
//   - "call"  一次性：一个 invoke 一个进程，写完 result 即退出（渠道插件的默认形态，无常驻）
//   - "stdio" 常连：进程常驻，stdin/stdout 上反复 invoke（拦截器插件用，按需拉起后常驻）
//
// 渠道插件两种形态都实现（core 一律用 call，CLI 自测用 stdio 复现同一条链路）；
// 字段留空视同 "call"（老插件兼容）。
type Execution struct {
	Control            string `json:"control"`                        // call | stdio
	Conversation       string `json:"conversation"`                   // call | stdio
	MaxWorkers         int    `json:"max_workers,omitempty"`          // 调用方并发上限提示（0=不限）
	IdleTimeoutSeconds int    `json:"idle_timeout_seconds,omitempty"` // stdio 常连形态的空闲回收秒数
}

// Capabilities 按角色分组的能力集。
type Capabilities struct {
	Control      []string `json:"control,omitempty"`
	Conversation []string `json:"conversation,omitempty"`
}

// HasControl 报告是否声明某控制面能力。
func (c Capabilities) HasControl(m string) bool { return contains(c.Control, m) }

// HasConversation 报告是否声明某会话面能力。
func (c Capabilities) HasConversation(m string) bool { return contains(c.Conversation, m) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ModelInfo 模型信息（models.list，PLUGIN_PROTOCOL §5.2）。允许 null 的字段用指针/零值区分。
type ModelInfo struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	Context      *int          `json:"context,omitempty"`
	InputPrice   *float64      `json:"input_price,omitempty"`
	OutputPrice  *float64      `json:"output_price,omitempty"`
	Capabilities Capabilities2 `json:"capabilities,omitempty"`
}

// Capabilities2 模型能力标记（勿臆造：未探测就不置 true）。
type Capabilities2 struct {
	Tools     bool `json:"tools,omitempty"`
	Vision    bool `json:"vision,omitempty"`
	Reasoning bool `json:"reasoning,omitempty"`
}

// ResourceItem 额度/积分项（旧版 provider.ResourceItem 语义，IPC_CHAT §5 / AGENTS §7.3）。
type ResourceItem struct {
	Name     string   `json:"name"`
	Total    *float64 `json:"total,omitempty"`
	Used     *float64 `json:"used,omitempty"`
	Remain   *float64 `json:"remain,omitempty"`
	Unit     string   `json:"unit,omitempty"` // 空=credits；"token"
	Usable   bool     `json:"usable,omitempty"`
	InfoOnly bool     `json:"info_only,omitempty"`
	Key      string   `json:"key,omitempty"`       // 稳定 ID 供 ledger 差分对账
	ExpireAt string   `json:"expire_at,omitempty"` // YYYY-MM-DD 墙钟日 或 RFC3339；空=未下发
}

// AccountInfo 账号视图（accounts.list）。
// Prefixes 是该账号可服务的模型命名空间前缀（workbuddy 系一插件双前缀 workbuddy/workbuddyai），
// core 据此在模型解析时把 `workbuddyai/gpt-5.5` 路由到正确的渠道/账号。
type AccountInfo struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname,omitempty"`
	Status    string   `json:"status,omitempty"` // ok | cooling | disabled | session_dead
	LoginKind string   `json:"login_kind,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	ChannelID string   `json:"channel_id,omitempty"`
	Region    string   `json:"region,omitempty"`
	Credits   *float64 `json:"credits,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Prefixes  []string `json:"prefixes,omitempty"`
	// AuthFile 是凭据标识（<data>/auths/<auth_file>.json，无后缀）。
	// 它是账号在主程序与插件之间的唯一稳定标示（UID 可能跨区重名，见 PLUGIN_CLI §5.2）。
	AuthFile string `json:"auth_file,omitempty"`
}
