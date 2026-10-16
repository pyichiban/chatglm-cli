// Package glm 是 chatglm.cn 私有接口的协议层（与 CLI 无关，可被其它 Go 程序复用）。
//
// 只走 **IDE(Electron) 通道**：WEB 通道已彻底放弃（画图族接口在 WEB 通道要 refer__991 动态风控参数，
// 无法离线复刻），而 IDE 主对话链路实测 100% 通过 ⇒ 本实现 **不需要 refer__991、不做退避重试**。
// 所有取值都有抓包实证，来源 docs/PROTOCOL.md、docs/生图协议与实操.md；改动前请回查。
package glm

// ── 上游与签名 ───────────────────────────────────────────────
const (
	BASE = "https://chatglm.cn"
	// SIGN_SECRET 是清言客户端内硬编码的签名密钥 [实证 PROTOCOL §2]。
	SIGN_SECRET = "8a1317a7468aa3ad86e997d08f3f31cb"
)

// ── 业务默认值 ───────────────────────────────────────────────
// assistant_id 是「智能体 ID」而非模型 ID [实证 PROTOCOL §4]。
// 常量名与 Python 侧 glm2api/constants.py 保持同名，便于做防漂移比对（见 cmd/constsdump）。
const (
	DEFAULT_ASSISTANT = "65940acff94777010aa6b796" // 主对话 ChatGLM
	ASSISTANT_DRAWING = "65a232c082ff90a2ad2f15e2" // AI 画图（生图=对话通道换 agent + cogview）
	ASSISTANT_READ    = "658a7988b8a9a98d38725745" // AI 阅读
	ASSISTANT_VIDEO   = "668d03b2e99d661ed3c32516" // 视频助手
	ASSISTANT_STUDY   = "68f0b8c110eea3e78b0e0e5e" // 学习搭子

	DEFAULT_MODEL             = "glm-5.3-flash"
	CHAT_TYPE                 = "user_chat"
	DEFAULT_PAGE_SIZE         = 20 // recent_list 网页版固定值
	DEFAULT_HISTORY_PAGE_SIZE = 30 // page_messages_v2 网页版固定值
)

// ── 端点 ────────────────────────────────────────────────────
const (
	EP_REFRESH   = "/chatglm/user-api/user/refresh"                   // POST，refresh 换 access，**会轮换 refresh**
	EP_USER_INFO = "/chatglm/user-api/user/info"                      // GET
	EP_MODELS    = "/chatglm/agent-api/operation/detail"              // GET ?tag=available_models
	EP_CONV_LIST = "/chatglm/mainchat-api/conversation/recent_list"   // POST {page,page_size}
	EP_CHAT      = "/chatglm/backend-api/assistant/stream"            // POST SSE
	EP_HISTORY   = "/chatglm/agent-api/conversation/page_messages_v2" // GET（❌ 死路：实测始终返回空 list）
	// EP_CONV_MESSAGES [实证 2026-10-16] 可用的单会话历史端点（PROTOCOL §3.5 的缺口由此补齐）。
	// ⚠️ assistant_id 必须与会话匹配（一个会话 = (assistant_id, conversation_id) 二元组），传错直接空列表。
	EP_CONV_MESSAGES = "/chatglm/mainchat-api/conversation/messages"     // GET ?conversation_id&assistant_id
	EP_BULK_DELETE   = "/chatglm/mainchat-api/conversation/bulk_delete"  // POST；单删也走 bulk
	EP_MODIFY_TITLE  = "/chatglm/mainchat-api/conversation/modify_title" // POST {conversation_id,title}
	EP_CONV_TITLE    = "/chatglm/mainchat-api/conversation/title"        // GET ?conversation_id
	EP_UPDATE_STATUS = "/chatglm/mainchat-api/stream/update_status"      // POST {history_id}
	EP_STOP_STREAM   = "/chatglm/mainchat-api/stream/stop_stream"        // POST {history_id}
)

// ── IDE 客户端指纹（逐头照抄，WAF 校验的是「头组合指纹」）─────
const (
	APP_NAME     = "chatglm"
	APP_PLATFORM = "win"   // IDE 是 win（WEB 是 pc）
	APP_VERSION  = "2.0.6" // IDE 客户端版本
	APP_FR       = "default"
	// IDE 的 UA 是 Electron，中文品牌段被客户端自己写坏成 ????（抓包原样保留，勿"修正"）。
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) ????/2.0.6 Chrome/150.0.7871.250 Electron/43.6.0 " +
		"Safari/537.36 Platform/ChatGLM_Win; AppVersion/2.0.6;DeviceId/{device_id};RefreshId/1790641614338"
	SEC_CH_UA = `"Not;A=Brand";v="8", "Chromium";v="150"`
	// EXP_GROUPS 服务端不校验取值但必须存在（WAF 指纹）[实证 PROTOCOL §2]。
	EXP_GROUPS = "na_android_config:exp:NA,na_4o_config:exp:4o_A,tts_config:exp:tts_config_a," +
		"na_glm4plus_config:exp:open,mainchat_server_app:exp:A,mobile_history_daycheck:exp:a," +
		"desktop_toolbar:exp:A,chat_drawing_server:exp:A,drawing_server_cogview:exp:cogview4," +
		"app_welcome_v2:exp:A,chat_drawing_streamv2:exp:A,mainchat_rm_fc:exp:add," +
		"mainchat_dr:exp:open,chat_auto_entrance:exp:A,drawing_server_hi_dream:control:A," +
		"homepage_square:exp:close,assistant_recommend_prompt:exp:3,app_home_regular_user:exp:A," +
		"mainchat_moe:exp:300,assistant_greet_user:exp:greet_user," +
		"app_welcome_personalize:exp:A,assistant_model_exp_group:exp:glm4.5,ai_wallet:exp:ai_wallet_enable"
)

// 非流式请求超时。流式请求**刻意不设超时**（PROTOCOL/HANDOFF 硬规则 2：设了会把长回答掐断）。
const (
	CONNECT_TIMEOUT = 15  // 秒
	READ_TIMEOUT    = 60  // 秒
	AUTH_SKEW       = 300 // access 提前 5 分钟视为过期，避免边界
)

// ── 业务码 ──────────────────────────────────────────────────
const (
	STATUS_TRANSIENT = 40012 // 阿里云 WAF 瞬时拒绝（HTTP 400）。IDE 链路实测不出现，出现即手动重试
	AUTH_STATUS_1    = 40102 // 登录态失效
	AUTH_STATUS_2    = 40103 // user needs login
)

// ── 环境变量 ────────────────────────────────────────────────
const (
	ENV_REFRESH_TOKEN   = "GLM_REFRESH_TOKEN"
	ENV_ACCESS_TOKEN    = "GLM_ACCESS_TOKEN"
	ENV_DEVICE_ID       = "GLM_DEVICE_ID"
	ENV_USER_ID         = "GLM_USER_ID"
	ENV_COOKIE          = "GLM_COOKIE"
	ENV_CREDENTIAL_FILE = "GLM_CREDENTIAL_FILE"
)

// 业务 cookie 与要丢弃的 CDN/WAF 浏览器指纹 cookie [实证 PROTOCOL §1.4]。
var (
	COOKIE_KEYS     = []string{"chatglm_token", "chatglm_refresh_token", "chatglm_user_id"}
	WAF_COOKIE_KEYS = []string{"acw_tc", "cdn_sec_tc", "chatglm_token_expires",
		"_c_WBKFRo", "_nb_ioWEgULi"}
	WAF_COOKIE_PREFIXES = []string{"ssxmod", "_c_", "_nb_"}
)

// AGENTS 返回内置智能体表（CLI 的 agents 子命令用）。
func AGENTS() []map[string]string {
	return []map[string]string{
		{"id": DEFAULT_ASSISTANT, "name": "ChatGLM 主对话", "alias": "main"},
		{"id": ASSISTANT_DRAWING, "name": "AI 画图", "alias": "drawing"},
		{"id": ASSISTANT_READ, "name": "AI 阅读", "alias": "read"},
		{"id": ASSISTANT_VIDEO, "name": "视频助手", "alias": "video"},
		{"id": ASSISTANT_STUDY, "name": "学习搭子", "alias": "study"},
	}
}
