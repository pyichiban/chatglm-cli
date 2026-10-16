package glm

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 跨语言契约测试：断言 Go 实现与 testdata/contract.json（同一份文件也被 Python 实现断言）一致。
//
// contract.json 的 fixtures 期望值来自 **另一套实现（Python）+ 真实抓包帧**，不是自我复制；
// known_divergences 显式记录两边目前不一致之处，各自断言自己那一项 ⇒ 改动必先改契约，防止悄悄漂移。

type contract struct {
	ContractVersion int `json:"contract_version"`
	Channel         struct {
		Name                   string   `json:"name"`
		AppName                string   `json:"app_name"`
		AppPlatform            string   `json:"app_platform"`
		AppVersion             string   `json:"app_version"`
		AppFr                  string   `json:"app_fr"`
		SecChUA                string   `json:"sec_ch_ua"`
		UAMarkers              []string `json:"ua_markers"`
		Refer991Required       bool     `json:"referer__991_required"`
		AcceptEncoding         string   `json:"accept_encoding"`
		ContentTypeOnBody      string   `json:"content_type_on_body"`
		ProxySupport           bool     `json:"proxy_support"`
		HeaderNames            []string `json:"header_names"`
		PerRequestExtraHeaders []string `json:"per_request_extra_headers"`
		NeverHeaders           []string `json:"never_headers"`
	} `json:"channel"`
	Sign struct {
		Secret   string `json:"secret"`
		NonceLen int    `json:"nonce_len"`
		TsLen    int    `json:"ts_len"`
		Vector   struct {
			Ts, Nonce, Sign string
		} `json:"vector"`
	} `json:"sign"`
	Endpoints map[string]string `json:"endpoints"`
	Agents    map[string]string `json:"agents"`
	Defaults  struct {
		Base              string `json:"base"`
		Model             string `json:"model"`
		ChatType          string `json:"chat_type"`
		PageSize          int    `json:"page_size"`
		HistoryPageSizeV2 int    `json:"history_page_size_v2"`
		MetaPlatform      string `json:"meta_platform"`
		CIDPattern        string `json:"cid_pattern"`
	} `json:"defaults"`
	Codes struct {
		Transient int   `json:"transient"`
		Auth      []int `json:"auth"`
	} `json:"codes"`
	Timeouts struct {
		ConnectSec    int    `json:"connect_sec"`
		ReadSec       int    `json:"read_sec"`
		AuthSkewSec   int    `json:"auth_skew_sec"`
		StreamTimeout *int   `json:"stream_total_timeout"`
		Rule          string `json:"rule"`
	} `json:"timeouts"`
	Env     map[string]string `json:"env"`
	Cookies struct {
		Business      []string `json:"business"`
		WAFDropKeys   []string `json:"waf_drop_keys"`
		WAFDropPrefix []string `json:"waf_drop_prefix"`
	} `json:"cookies"`
	CredFile struct {
		Keys                []string `json:"keys"`
		Write               string   `json:"write"`
		PreserveUnknownKeys bool     `json:"preserve_unknown_keys"`
		MustPersist         bool     `json:"must_persist_after_refresh"`
	} `json:"credential_file"`
	Retry struct {
		Enabled bool   `json:"enabled"`
		Rule    string `json:"rule"`
	} `json:"retry"`
	Note       string `json:"note"`
	Pagination struct {
		ConversationList     string `json:"conversation_list"`
		ConversationMessages string `json:"conversation_messages"`
		Rule                 string `json:"rule"`
	} `json:"pagination"`
	ChatBody struct {
		TopKeys           []string `json:"top_keys"`
		MetaKeys          []string `json:"meta_keys"`
		CogviewBase       []string `json:"cogview_base_keys"`
		CogviewDrawing    []string `json:"cogview_drawing_keys"`
		MessagesLen       int      `json:"messages_len"`
		FirstTurnCID      string   `json:"first_turn_conversation_id"`
		ReasoningOptional bool     `json:"reasoning_effort_optional"`
	} `json:"chat_body"`
	RequestShapes map[string]struct {
		Method     string         `json:"method"`
		Endpoint   string         `json:"endpoint"`
		Query      []string       `json:"query"`
		BodyKeys   []string       `json:"body_keys"`
		Pagination []string       `json:"pagination_params"`
		Body       map[string]any `json:"body"`
	} `json:"request_shapes"`
	SSERules struct {
		DoneSentinel bool     `json:"done_sentinel"`
		FinishMarker string   `json:"finish_marker"`
		TextTypes    []string `json:"content_text_types"`
		ThinkTypes   []string `json:"content_think_types"`
		ImageType    string   `json:"content_image_type"`
		PartKeyOrder []string `json:"part_key_precedence"`
		Truncation   string   `json:"truncation"`
		UsageFields  *string  `json:"usage_fields"`
	} `json:"sse_rules"`
	CLIEvents struct {
		Version int                 `json:"version"`
		Types   []string            `json:"types"`
		First   string              `json:"first_event"`
		Fields  map[string][]string `json:"fields"`
	} `json:"cli_event_contract"`
	ExitCodes   map[string]int             `json:"exit_codes"`
	Fixtures    map[string]json.RawMessage `json:"fixtures"`
	Divergences []divJSON                  `json:"known_divergences"`
}

type fixtureWant struct {
	ConversationID string `json:"conversation_id"`
	HistoryID      string `json:"history_id"`
	Frames         int    `json:"frames"`
	Finished       bool   `json:"finished"`
	ErrorCode      *int   `json:"error_code"`
	TextLen        int    `json:"text_len"`
	TextSHA        string `json:"text_sha"`
	TextHead       string `json:"text_head"`
	ReasoningLen   int    `json:"reasoning_len"`
	ReasoningSHA   string `json:"reasoning_sha"`
	ImageCount     int    `json:"image_count"`
	ImageSHA       string `json:"image_sha"`
	ImageURLHead   string `json:"image_url_head"`
	Point          string `json:"point"`
}

func loadContract(t *testing.T) (*contract, map[string]fixtureWant) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contract.json"))
	if err != nil {
		t.Fatalf("读契约文件失败: %v", err)
	}
	var c contract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("契约 JSON 解析失败: %v", err)
	}
	want := map[string]fixtureWant{}
	for k, v := range c.Fixtures {
		if !strings.HasPrefix(k, "stream_") {
			continue
		}
		var w fixtureWant
		if err := json.Unmarshal(v, &w); err != nil {
			t.Fatalf("fixtures.%s 解析失败: %v", k, err)
		}
		want[k] = w
	}
	return &c, want
}

func fixtureSHA(t *testing.T, c *contract) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(c.Fixtures["sha256"], &m); err != nil {
		t.Fatalf("fixtures.sha256 解析失败: %v", err)
	}
	return m
}

func sha16(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func sortedStr(xs []string) string {
	c := append([]string(nil), xs...)
	sort.Strings(c)
	return strings.Join(c, ",")
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) < n {
		n = len(r)
	}
	return string(r[:n])
}

func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

type div struct{ ID, Topic, Python, Go, Note string }

type divJSON struct {
	ID     string `json:"id"`
	Topic  string `json:"topic"`
	Python string `json:"python"`
	Go     string `json:"go"`
	Note   string `json:"note"`
}

func divergence(t *testing.T, c *contract, id string) (d div) {
	t.Helper()
	for _, x := range c.Divergences {
		if x.ID == id {
			return div{ID: x.ID, Topic: x.Topic, Python: x.Python, Go: x.Go, Note: x.Note}
		}
	}
	t.Fatalf("契约里缺差异条目 %s", id)
	return
}

func newTestClient(t *testing.T, base string) *Client {
	t.Helper()
	cred := MakeCredential(_testJWT(), "", "", "", "")
	cred.AccessToken = _testJWT()
	cl, err := NewClient(t.Context(), cred, Opt{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// 只要求形状合法（Go 只 base64 解 payload 不验签）；exp 远期避免测试触发刷新。
func _testJWT() string {
	return "eyJhbGciOiJIUzI1NiJ9." + b64url(
		`{"uid":"6982e0ad0efeac7998789a48","device_id":"d4f911aff0054012b4bd069683860b35","exp":4102444800}`) + ".sig"
}

func keysOf[T any](m map[string]T) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── fixture 与契约文件本身必须两边同源 ─────────────────────
func TestContractFixtureBytesAreShared(t *testing.T) {
	c, _ := loadContract(t)
	for name, want := range fixtureSHA(t, c) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("缺 fixture %s: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s 与契约 sha256 不一致 ⇒ 两边回放的已经不是同一份流量", name)
		}
	}
}

// ── 常量 / 端点 / 码值 / cookie / env ──────────────────────
func TestContractConstants(t *testing.T) {
	c, _ := loadContract(t)
	got := map[string]string{"refresh": EP_REFRESH, "user_info": EP_USER_INFO, "models": EP_MODELS,
		"conv_list": EP_CONV_LIST, "chat": EP_CHAT, "history": EP_CONV_MESSAGES,
		"history_v2_dead": EP_HISTORY, "bulk_delete": EP_BULK_DELETE, "modify_title": EP_MODIFY_TITLE,
		"conv_title": EP_CONV_TITLE, "update_status": EP_UPDATE_STATUS, "stop_stream": EP_STOP_STREAM,
		"edit": EP_IMAGE_REFERENCE,
	}
	if !reflect.DeepEqual(got, c.Endpoints) {
		t.Errorf("端点表与契约不一致: got=%v want=%v", got, c.Endpoints)
	}
	if BASE != c.Defaults.Base || DEFAULT_MODEL != c.Defaults.Model || CHAT_TYPE != c.Defaults.ChatType {
		t.Errorf("defaults 不一致: %s %s %s", BASE, DEFAULT_MODEL, CHAT_TYPE)
	}
	if DEFAULT_PAGE_SIZE != c.Defaults.PageSize || DEFAULT_HISTORY_PAGE_SIZE != c.Defaults.HistoryPageSizeV2 {
		t.Errorf("分页默认值不一致")
	}
	if STATUS_TRANSIENT != c.Codes.Transient || AUTH_STATUS_1 != c.Codes.Auth[0] || AUTH_STATUS_2 != c.Codes.Auth[1] {
		t.Errorf("业务码不一致")
	}
	if SIGN_SECRET != c.Sign.Secret {
		t.Errorf("签名密钥不一致")
	}
	if CONNECT_TIMEOUT != c.Timeouts.ConnectSec || READ_TIMEOUT != c.Timeouts.ReadSec || AUTH_SKEW != c.Timeouts.AuthSkewSec {
		t.Errorf("超时常量不一致")
	}
	if c.Timeouts.StreamTimeout != nil {
		t.Errorf("契约里流式超时必须为 null（不设总超时）")
	}
	if sortedStr(COOKIE_KEYS) != sortedStr(c.Cookies.Business) ||
		sortedStr(WAF_COOKIE_KEYS) != sortedStr(c.Cookies.WAFDropKeys) ||
		sortedStr(WAF_COOKIE_PREFIXES) != sortedStr(c.Cookies.WAFDropPrefix) {
		t.Errorf("cookie 名单不一致")
	}
	env := map[string]string{"refresh_token": ENV_REFRESH_TOKEN, "access_token": ENV_ACCESS_TOKEN,
		"device_id": ENV_DEVICE_ID, "user_id": ENV_USER_ID, "cookie": ENV_COOKIE,
		"credential_file": ENV_CREDENTIAL_FILE}
	if !reflect.DeepEqual(env, c.Env) {
		t.Errorf("环境变量名不一致: %v", env)
	}
	if !reflect.DeepEqual(map[string]string{"main": DEFAULT_ASSISTANT, "drawing": ASSISTANT_DRAWING,
		"read": ASSISTANT_READ, "video": ASSISTANT_VIDEO, "study": ASSISTANT_STUDY}, c.Agents) {
		t.Errorf("内置 agent 表与契约不一致")
	}
}

// ── 签名与 IDE 指纹 ───────────────────────────────────────
func TestContractSignAndHeaders(t *testing.T) {
	c, _ := loadContract(t)
	if Sign(1790732179905, c.Sign.Vector.Nonce) != c.Sign.Vector.Sign {
		t.Errorf("签名复算与契约向量不符")
	}
	h := Headers("T", "text/event-stream", "dev1")
	if len(h["X-Nonce"]) != c.Sign.NonceLen || len(h["X-Timestamp"]) != c.Sign.TsLen || len(h["X-Sign"]) != 32 {
		t.Errorf("签名三件套长度异常")
	}
	for _, k := range c.Channel.HeaderNames {
		if _, ok := h[k]; !ok {
			t.Errorf("缺少头 %s", k)
		}
	}
	if h["App-Name"] != c.Channel.AppName || h["X-App-Platform"] != c.Channel.AppPlatform ||
		h["X-App-Version"] != c.Channel.AppVersion || h["X-App-fr"] != c.Channel.AppFr ||
		h["sec-ch-ua"] != c.Channel.SecChUA {
		t.Errorf("IDE 指纹头值不一致: %s/%s/%s", h["X-App-Platform"], h["X-App-Version"], h["X-App-fr"])
	}
	for _, m := range c.Channel.UAMarkers {
		if !strings.Contains(h["User-Agent"], m) {
			t.Errorf("UA 缺标记 %s", m)
		}
	}
	for _, k := range c.Channel.NeverHeaders {
		if _, ok := h[k]; ok {
			t.Errorf("%s 必须由 cookie jar 管理，不能手写进头", k)
		}
	}
	if _, ok := h["X-Device-Id"]; !ok {
		t.Errorf("有 device_id 时必须带 X-Device-Id")
	}
	if len(h["X-Request-Id"]) != 32 {
		t.Errorf("X-Request-Id 应为 32 hex")
	}
	// V3/V2/V12 已对齐（contract v2）：Accept-Encoding 只有 gzip/deflate；
	// IDE 通道无 refer__991；不支持代理
	if h["Accept-Encoding"] != c.Channel.AcceptEncoding {
		t.Errorf("Accept-Encoding = %q，期望 %q", h["Accept-Encoding"], c.Channel.AcceptEncoding)
	}
	if c.Channel.Refer991Required || c.Channel.ProxySupport {
		t.Errorf("契约：IDE 通道无 refer__991、不支持代理")
	}
	// V4 已对齐：有 body 时发 Content-Type（线上头在 TestContractRequestShapes 断言）
	if c.Channel.ContentTypeOnBody != "application/json;charset=UTF-8" {
		t.Errorf("契约 content_type_on_body = %q", c.Channel.ContentTypeOnBody)
	}
}

func TestContractHeadersWithoutDevice(t *testing.T) {
	h := Headers("", "", "")
	if _, ok := h["Authorization"]; ok {
		t.Errorf("空 token 不该带 Authorization")
	}
	if _, ok := h["X-Device-Id"]; ok {
		t.Errorf("空 device_id 不该带 X-Device-Id")
	}
	if !strings.Contains(h["User-Agent"], "Electron/") {
		t.Errorf("UA 应保持 Electron 指纹: %q", h["User-Agent"])
	}
}

// ── 请求体形状 ───────────────────────────────────────────
func TestContractChatBody(t *testing.T) {
	c, _ := loadContract(t)
	b := ChatBody(ChatOpt{Prompt: "你好"})
	if sortedStr(keysOf(b)) != sortedStr(c.ChatBody.TopKeys) {
		t.Errorf("顶层键 = %s，期望 %s", sortedStr(keysOf(b)), sortedStr(c.ChatBody.TopKeys))
	}
	if b["conversation_id"] != c.ChatBody.FirstTurnCID || b["project_id"] != "" {
		t.Errorf("首轮字段错")
	}
	msgs, _ := b["messages"].([]any)
	if len(msgs) != c.ChatBody.MessagesLen {
		t.Fatalf("messages 必须只有当前这一条，得到 %d", len(msgs))
	}
	meta, _ := b["meta_data"].(map[string]any)
	if sortedStr(keysOf(meta)) != sortedStr(c.ChatBody.MetaKeys) {
		t.Errorf("meta_data 键 = %s，期望 %s", sortedStr(keysOf(meta)), sortedStr(c.ChatBody.MetaKeys))
	}
	if meta["platform"] != c.Defaults.MetaPlatform {
		t.Errorf("platform = %v（头部是 win，body 必须是 pc）", meta["platform"])
	}
	cw, _ := meta["cogview"].(map[string]any)
	if sortedStr(keysOf(cw)) != sortedStr(c.ChatBody.CogviewBase) {
		t.Errorf("普通对话 cogview 键 = %s", sortedStr(keysOf(cw)))
	}
	if _, ok := meta["reasoning_effort"]; ok {
		t.Errorf("不传 effort 就不该发（契约: reasoning_effort_optional）")
	}
	// 生图：换 agent + cogview 三键（⚠️ V7：Python 总带 style/scene=none，Go 只在显式传入时带）
	b2 := ChatBody(ChatOpt{Agent: ASSISTANT_DRAWING, Prompt: "画只猫", Ratio: "3:4", Style: "ghibli", Scene: "x"})
	if b2["assistant_id"] != c.Agents["drawing"] {
		t.Errorf("生图 agent 错")
	}
	cw2 := b2["meta_data"].(map[string]any)["cogview"].(map[string]any)
	need := append(append([]string{}, c.ChatBody.CogviewBase...), c.ChatBody.CogviewDrawing...)
	if sortedStr(keysOf(cw2)) != sortedStr(need) {
		t.Errorf("生图 cogview 键 = %s，期望 %s", sortedStr(keysOf(cw2)), sortedStr(need))
	}
	// 多轮：唯一差别就是 conversation_id
	b3 := ChatBody(ChatOpt{Prompt: "第二问", CID: strings.Repeat("c", 24)})
	if b3["conversation_id"] != strings.Repeat("c", 24) || len(b3["messages"].([]any)) != 1 {
		t.Errorf("多轮请求体错")
	}
	// V7：Go 侧只发显式给过的参数，不自动补 style/scene
	b4 := ChatBody(ChatOpt{Prompt: "x", Ratio: "1:1"})
	cw4 := b4["meta_data"].(map[string]any)["cogview"].(map[string]any)
	if _, ok := cw4["style"]; ok {
		t.Errorf("对齐后：Go 不应自动补 style/scene（生图入口在 CLI --ratio）")
	}
}

// ── 端点请求形状（httptest，绝不连真上游）─────────────────
func TestContractRequestShapes(t *testing.T) {
	c, _ := loadContract(t)
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
		}
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		got = append(got, recorded{r.Method, r.URL.Path, q, body, r.Header.Clone()})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":0,"message":"success","result":{}}`)
	}))
	defer srv.Close()

	cl := newTestClient(t, srv.URL)
	cid, aid, mid := strings.Repeat("c", 24), strings.Repeat("a", 24), strings.Repeat("h", 24)
	var errs []string
	chk := func(_ json.RawMessage, err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	chk(cl.Rename(cid, "T"))
	chk(cl.ConvTitle(cid))
	chk(cl.BulkDelete([]string{cid}, aid))
	chk(cl.StopStream(mid))
	chk(cl.UpdateStatus(mid))
	chk(cl.ConvList(1, c.Defaults.PageSize))
	chk(cl.History(cid, aid, nil))
	chk(cl.HistoryV2(cid, aid, c.Defaults.HistoryPageSizeV2, nil))
	chk(cl.Models())
	chk(cl.UserInfo())
	if len(errs) > 0 {
		t.Fatalf("请求失败: %v", errs)
	}

	byPath := map[string]recorded{}
	for _, g := range got {
		byPath[g.path] = g
	}
	for name, want := range c.RequestShapes {
		if name == "refresh" || name == "chat" || name == "edit" { // refresh/edit 有专属测试
			continue
		}
		ep := c.Endpoints[want.Endpoint]
		g, ok := byPath[ep]
		if !ok {
			t.Errorf("%s 没打到 %s（实际打到: %v）", name, ep, keysOf(byPath))
			continue
		}
		if g.method != want.Method {
			t.Errorf("%s method = %s，期望 %s", name, g.method, want.Method)
		}
		if len(want.Query) > 0 && sortedStr(keysOf(g.query)) != sortedStr(want.Query) {
			t.Errorf("%s query = %s，期望 %s", name, sortedStr(keysOf(g.query)), sortedStr(want.Query))
		}
		if len(want.BodyKeys) > 0 && sortedStr(keysOf(g.body)) != sortedStr(want.BodyKeys) {
			t.Errorf("%s body 键 = %s，期望 %s", name, sortedStr(keysOf(g.body)), sortedStr(want.BodyKeys))
		}
		if name == "history" && len(want.Pagination) == 0 {
			for k := range g.query {
				if k == "page" || k == "page_size" || k == "cursor" {
					t.Errorf("history 已实证无分页，不该发 %s", k)
				}
			}
		}
	}
	// V4 已对齐：Go 有 body 时显式发 Content-Type（与抓包一致）
	if byPath[c.Endpoints["modify_title"]].hdr.Get("Content-Type") != c.Channel.ContentTypeOnBody {
		t.Errorf("有 body 的请求必须发 Content-Type=%q", c.Channel.ContentTypeOnBody)
	}
}

type recorded struct {
	method, path string
	query        map[string]string
	body         map[string]any
	hdr          http.Header
}

// ── refresh：Authorization 用根凭据、body={}、轮换必落盘 ───
func TestContractRefreshShapeAndPersist(t *testing.T) {
	c, _ := loadContract(t)
	dir := t.TempDir()
	credPath := filepath.Join(dir, "cred.json")
	os.WriteFile(credPath, []byte(`{"refresh_token":"old-root-token","refer_991":"keep-me"}`), 0o600)

	newRoot := "eyJhbGciOiJIUzI1NiJ9." + b64url(`{"uid":"u1","exp":4102444800}`) + ".sig"
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, recorded{r.Method, r.URL.Path, nil,
			map[string]any{"_raw": string(b)}, r.Header.Clone()})
		fmt.Fprintf(w, `{"status":0,"message":"success","result":{"access_token":%q,
			"refresh_token":%q,"user_id":"u1","is_guest":false}}`,
			"eyJhbGciOiJIUzI1NiJ9."+b64url(`{"uid":"u1","exp":4102444800}`)+".sig", newRoot)
	}))
	defer srv.Close()

	cred, err := LoadCredential(credPath)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := NewClient(t.Context(), cred, Opt{Base: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.EnsureToken(true); err != nil {
		t.Fatalf("refresh 失败: %v", err)
	}
	if len(calls) != 1 || calls[0].path != c.Endpoints["refresh"] {
		t.Fatalf("应打 %s 一次，得到 %+v", c.Endpoints["refresh"], calls)
	}
	if got := calls[0].hdr.Get("Authorization"); got != "Bearer old-root-token" {
		t.Errorf("refresh 的 Authorization = %q（必须是根凭据）", got)
	}
	if calls[0].body["_raw"] != "{}" {
		t.Errorf("refresh body = %v，期望 {}", calls[0].body["_raw"])
	}
	if cred.RefreshToken != newRoot {
		t.Errorf("轮换后的 refresh 未进内存")
	}
	onDisk, _ := os.ReadFile(credPath)
	var m map[string]any
	if json.Unmarshal(onDisk, &m) != nil || m["refresh_token"] != newRoot {
		t.Errorf("轮换后的 refresh 必须落盘（铁律 1）: %s", onDisk)
	}
	for _, k := range c.CredFile.Keys {
		if _, ok := m[k]; !ok {
			t.Errorf("写盘缺键 %s（契约 keys=%v）", k, c.CredFile.Keys)
		}
	}
	// V5 已对齐：Go 保留未知键，与 Python/其它工具共用凭据文件不丢数据
	if _, ok := m["refer_991"]; !ok {
		t.Errorf("未知键必须保留（credential_file.preserve_unknown_keys）")
	}
	if !c.CredFile.PreserveUnknownKeys {
		t.Errorf("契约必须声明 preserve_unknown_keys=true")
	}
}

// ── 错误映射与「不重试」（V1）──────────────────────────────
func TestContractTransientNoRetry(t *testing.T) {
	c, _ := loadContract(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":40012,"message":"bad request"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	cl := newTestClient(t, srv.URL)
	_, err := cl.UserInfo()
	ge, ok := err.(*Error)
	if !ok || ge.Code != CTransient {
		t.Fatalf("应翻译成 transient_rejection，得到 %v", err)
	}
	if int(ge.Code) != c.ExitCodes["transient_rejection"] {
		t.Errorf("退出码 = %d，契约 %d", ge.Code, c.ExitCodes["transient_rejection"])
	}
	if !ge.Code.Retryable() {
		t.Errorf("transient 应标 retryable")
	}
	// V1 已对齐（retry.enabled=false）：40012 只打一次，不重试
	if c.Retry.Enabled {
		t.Errorf("契约 retry.enabled 必须为 false（IDE 链路不退避）")
	}
	d := divergence(t, c, "V11") // 差异清单只剩有意为之的项
	if !strings.Contains(d.Python, "调试入口") {
		t.Errorf("V11 应描述 Python 为有意子集: %+v", d)
	}
	d15 := divergence(t, c, "V15")
	if !strings.Contains(d15.Note, "抓包") {
		t.Errorf("V15 应说明改动前置条件（需抓包验证）: %+v", d15)
	}
}

func TestContractErrorMapping(t *testing.T) {
	c, _ := loadContract(t)
	for _, tc := range []struct {
		body string
		want Code
	}{{`{"status":40103,"message":"user needs login"}`, CAuth},
		{`{"status":40102,"message":"x"}`, CAuth},
		{`{"status":1,"message":"invalid"}`, CUpstream}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, tc.body)
		}))
		cl := newTestClient(t, srv.URL)
		_, err := cl.UserInfo()
		srv.Close()
		ge, ok := err.(*Error)
		if !ok || ge.Code != tc.want {
			t.Errorf("%s ⇒ 期望 %v，得到 %v", tc.body, tc.want, err)
		}
		if int(tc.want) != c.ExitCodes[tc.want.String()] {
			t.Errorf("%v 的退出码与契约不一致", tc.want)
		}
	}
}

// ── SSE 语义：Go 必须与 Python 在同一份真实帧上给出同一组数字 ──
func TestContractSSEFixtures(t *testing.T) {
	_, want := loadContract(t)
	for name, w := range want {
		f, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		agg, rerr := ReadStream(strings.NewReader(string(f)), nil, nil)
		if agg.ConversationID != w.ConversationID {
			t.Errorf("%s conversation_id = %q，期望 %q", name, agg.ConversationID, w.ConversationID)
		}
		if agg.MessageID != w.HistoryID {
			t.Errorf("%s message_id = %q，期望 %q", name, agg.MessageID, w.HistoryID)
		}
		if agg.Frames != w.Frames {
			t.Errorf("%s frames = %d，期望 %d", name, agg.Frames, w.Frames)
		}
		if agg.Finished != w.Finished {
			t.Errorf("%s finished = %v，期望 %v", name, agg.Finished, w.Finished)
		}
		code := 0
		if agg.Err != nil {
			code = agg.Err.ErrorCode
		}
		if w.ErrorCode != nil && code != *w.ErrorCode {
			t.Errorf("%s error_code = %d，期望 %d", name, code, *w.ErrorCode)
		}
		// Go 的正文不含图片 Markdown ⇒ 直接对基准（V6：Python 会把图链拼进 text）
		// 长度口径与 Python 一致：按字符数（rune）而非字节（与 done.text_chars 同口径）
		text := agg.Text()
		if len([]rune(text)) != w.TextLen || sha16(text) != w.TextSHA {
			t.Errorf("%s 正文不一致: len=%d sha=%s；期望 len=%d sha=%s",
				name, len([]rune(text)), sha16(text), w.TextLen, w.TextSHA)
		}
		if w.TextHead != "" && !strings.HasPrefix(text, w.TextHead) {
			t.Errorf("%s 正文开头不符: %q", name, firstN(text, 24))
		}
		reasoning := agg.Reasoning()
		if len([]rune(reasoning)) != w.ReasoningLen || sha16(reasoning) != w.ReasoningSHA {
			t.Errorf("%s 思考不一致: len=%d sha=%s", name, len([]rune(reasoning)), sha16(reasoning))
		}
		urls := append([]string{}, agg.Images...)
		if len(urls) != w.ImageCount || sha16(strings.Join(urls, "|")) != w.ImageSHA {
			t.Errorf("%s 图片数/哈希不符: %d %s（期望 %d %s）",
				name, len(urls), sha16(strings.Join(urls, "|")), w.ImageCount, w.ImageSHA)
		}
		if w.ImageURLHead != "" && firstN(urls[0], 48) != w.ImageURLHead {
			t.Errorf("%s 首图链前缀 = %q", name, firstN(urls[0], 48))
		}
		// V6 已对齐后钉住：Go 侧生图产物是独立 image 列表/事件，正文不拼 Markdown
		if strings.Contains(text, "![") {
			t.Errorf("%s：Go 正文不应拼图片 Markdown（生图暴露形式以 Go 为准）", name)
		}
		switch name {
		case "stream_error.txt":
			if rerr != nil {
				t.Errorf("error 收尾不该算截断: %v", rerr)
			}
		default:
			if rerr != nil {
				t.Errorf("%s 应正常收尾: %v", name, rerr)
			}
		}
	}
}

// ── 收尾/截断/类型分桶规则 ────────────────────────────────
func TestContractSSERules(t *testing.T) {
	c, _ := loadContract(t)
	if c.SSERules.DoneSentinel {
		t.Errorf("上游不发 [DONE]")
	}
	if c.SSERules.UsageFields != nil {
		t.Errorf("上游没有任何 usage/计费字段")
	}
	if sortedStr(keysOf(textTypes)) != sortedStr(c.SSERules.TextTypes) ||
		sortedStr(keysOf(thinkTypes)) != sortedStr(c.SSERules.ThinkTypes) {
		t.Errorf("内容类型分桶与契约不一致")
	}
	if c.SSERules.ImageType != "image" {
		t.Errorf("图片类型 = %s", c.SSERules.ImageType)
	}
	if partKey(Part{LogicID: "L", ID: "I"}, 0) != "L" || partKey(Part{ID: "I"}, 1) != "I" ||
		partKey(Part{}, 3) != "#3" {
		t.Errorf("段落键优先级应为 %v", c.SSERules.PartKeyOrder)
	}
	_, err := ReadStream(strings.NewReader("data: {\"status\":\"init\",\"parts\":["+
		"{\"logic_id\":\"L\",\"status\":\"init\",\"content\":[{\"type\":\"text\",\"text\":\"半句\"}]}]}\n\n"), nil, nil)
	ge, ok := err.(*Error)
	if !ok || ge.Code != CStreamTruncated || int(ge.Code) != c.ExitCodes["stream_truncated"] {
		t.Fatalf("截断应报 stream_truncated(5)，得到 %v", err)
	}
	if !ge.Code.Retryable() {
		t.Errorf("截断必须标可重试")
	}
	if !strings.Contains(c.Timeouts.Rule, "不得设") {
		t.Errorf("契约的超时规则描述丢了")
	}
}

// ── CLI 事件契约：字段名（Python 侧只校验契约自身的完整性）───
func TestContractEventFieldNames(t *testing.T) {
	c, _ := loadContract(t)
	if EV != c.CLIEvents.Version {
		t.Errorf("事件版本 = %d，契约 %d", EV, c.CLIEvents.Version)
	}
	if c.CLIEvents.First != "meta" {
		t.Errorf("首个事件必须是 meta")
	}
	for typ, want := range c.CLIEvents.Fields {
		keys := jsonKeysOf(t, sampleEvent(typ))
		if sortedStr(keys) != sortedStr(want) {
			t.Errorf("%s 事件字段 = %s，期望 %s", typ, sortedStr(keys), sortedStr(want))
		}
	}
	for name, code := range c.ExitCodes {
		if name == "ok" {
			if code != 0 {
				t.Errorf("ok 必须是 0")
			}
			continue
		}
		if Code(code).String() != name {
			t.Errorf("退出码名 %s 与 Code(%d)=%s 不符", name, code, Code(code).String())
		}
	}
}

// sampleEvent 造出某类型事件实际会输出的字段（与 internal/cli 里的构造保持一致）。
func sampleEvent(typ string) *Event {
	switch typ {
	case "meta":
		return &Event{V: EV, Type: "meta", RequestID: "r", ConversationID: "c", MessageID: "m",
			AgentID: "a", Model: "glm", New: true, Cmd: "chat", TS: 1}
	case "text", "reasoning":
		return &Event{V: EV, Type: typ, RequestID: "r", Part: "p", Delta: "d"}
	case "image":
		return &Event{V: EV, Type: "image", RequestID: "r", Part: "p", URL: "u",
			PolishedPrompt: "pp", Index: 1}
	case "done":
		return &Event{V: EV, Type: "done", RequestID: "r", ConversationID: "c", MessageID: "m",
			AgentID: "a", Model: "glm", Cmd: "chat", New: true, Frames: 1, ElapsedMS: 1,
			Finished: true, Images: []string{"u"}, TextChars: 1, ReasoningChars: 1}
	case "error":
		return (&Event{}).Err(&Error{Code: CUpstream, Message: "x", Detail: "d",
			HTTPStatus: 400, BizStatus: 40012, RequestID: "r"})
	}
	return &Event{V: EV, Type: typ}
}

func jsonKeysOf(t *testing.T, e *Event) []string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return keysOf(m)
}

// ── 凭据文件与轮换持久化（V5 的 Go 侧）────────────────────
func TestContractCredentialFile(t *testing.T) {
	c, _ := loadContract(t)
	path := filepath.Join(t.TempDir(), "glm_credential.json")
	// 与 Python 共用同一份文件：未知键必须原样保留（V5：Python 写盘会丢掉未知键）
	os.WriteFile(path, []byte(`{"refresh_token":"old","refer_991":"keep"}`), 0o600)
	cred, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	cred.AccessToken, cred.RefreshToken = _testJWT(), _testJWT()
	if err := cred.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var onDisk map[string]json.RawMessage
	if json.Unmarshal(b, &onDisk) != nil {
		t.Fatalf("写回的 JSON 不合法: %s", b)
	}
	for _, k := range c.CredFile.Keys {
		if _, ok := onDisk[k]; !ok {
			t.Errorf("写盘缺键 %s（契约 keys=%v）", k, c.CredFile.Keys)
		}
	}
	if _, ok := onDisk["refer_991"]; !ok {
		t.Errorf("V5：Go 侧必须保留未知键，别把 Python 侧的数据抹掉")
	}
}

// ── 历史端点解析（V9：Go 规范化，Python 只透传）──────────
func TestContractHistoryShape(t *testing.T) {
	c, _ := loadContract(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		ConversationID string   `json:"conversation_id"`
		Turns          int      `json:"turns"`
		MessageIDs     []string `json:"message_ids"`
		CreatedAtHead  string   `json:"created_at_head"`
		TextSHA        string   `json:"text_sha"`
		UserSHA        string   `json:"user_sha"`
	}
	if err := json.Unmarshal(c.Fixtures["history.json"], &want); err != nil {
		t.Fatal(err)
	}
	turns, err := ParseHistory(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != want.Turns {
		t.Fatalf("轮数 = %d，期望 %d", len(turns), want.Turns)
	}
	var ids, texts, users []string
	for _, t := range turns {
		ids = append(ids, t.MessageID)
		texts = append(texts, t.Text)
		users = append(users, t.User)
	}
	if !reflect.DeepEqual(ids, want.MessageIDs) {
		t.Errorf("message_ids = %v", ids)
	}
	if turns[0].CreatedAt != want.CreatedAtHead {
		t.Errorf("created_at = %q", turns[0].CreatedAt)
	}
	if sha16(strings.Join(texts, "\n")) != want.TextSHA {
		t.Errorf("助手正文聚合与契约不符: %q", texts)
	}
	if sha16(strings.Join(users, "\n")) != want.UserSHA {
		t.Errorf("用户输入聚合与契约不符: %q", users)
	}
	if turns[0].User != "只回复两个字：收到" || turns[0].Text != "收到" {
		t.Errorf("第一轮解析错: %+v", turns[0])
	}
	// V9 已对齐：两仓都提供「原样 + 规范化」两层 API（Python history_turns / Go ParseHistory）
	if !strings.Contains(c.Note, "对齐") {
		t.Errorf("契约 note 应记录 2026-10-16 对齐轮次")
	}
}

// ── 差异清单本身的完整性 ─────────────────────────────────
func TestContractDivergencesComplete(t *testing.T) {
	c, _ := loadContract(t)
	ids := []string{}
	for _, d := range c.Divergences {
		if d.ID == "" || d.Topic == "" || d.Python == "" || d.Go == "" {
			t.Errorf("差异条目缺字段: %+v", d)
		}
		ids = append(ids, d.ID)
	}
	if len(ids) != len(unique(ids)) {
		t.Errorf("差异条目 id 重复: %v", ids)
	}
}

func unique(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ── V14：会话列表必须内部翻页，对外一次返回完整列表 ────────
func TestContractConvListAllPaginates(t *testing.T) {
	c, _ := loadContract(t)
	if !strings.Contains(c.Pagination.ConversationList, "内部") {
		t.Errorf("契约 pagination.conversation_list 应描述内部翻页")
	}
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		var list string
		switch pages {
		case 1:
			list = `[{"conversation_id":"` + strings.Repeat("a", 24) + `","title":"P1"}]`
		default:
			list = `[{"conversation_id":"` + strings.Repeat("b", 24) + `","title":"P2"}]`
		}
		fmt.Fprintf(w, `{"status":0,"result":{"has_more":%v,"conversation_list":%s}}`, pages == 1, list)
	}))
	defer srv.Close()

	cl := newTestClient(t, srv.URL)
	all, err := cl.ConvListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || pages != 2 {
		t.Fatalf("应内部翻 2 页拿全量 2 条，得到 %d 条 / %d 页", len(all), pages)
	}
	// 请求体形状不变（仍走原始单页端点 {page,page_size}）
	want := c.RequestShapes["conv_list"]
	if want.Method != "POST" || c.Endpoints[want.Endpoint] != EP_CONV_LIST {
		t.Errorf("conv_list 形状与契约不符")
	}
}

// ── edit（参考图生图）：端点请求形状，契约 v3 新增 ─────────
func TestContractEditShape(t *testing.T) {
	c, _ := loadContract(t)
	want := c.RequestShapes["edit"]
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		fmt.Fprintf(w, `{"status":0,"result":{"image_url":"https://sfile/x_0.jpg","conversation_id":%q}}`,
			strings.Repeat("c", 24))
	}))
	defer srv.Close()
	cl := newTestClient(t, srv.URL)
	raw, err := cl.EditImage("https://host/ref.jpg", "把背景改成夜空", "", "1:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody) == 0 {
		t.Fatalf("没打到端点")
	}
	keys := keysOfAny(gotBody)
	if sortedStr(keys) != sortedStr(want.BodyKeys) {
		t.Errorf("edit body 键 = %s，期望 %s", sortedStr(keys), sortedStr(want.BodyKeys))
	}
	if gotBody["image_url"] != "https://host/ref.jpg" || gotBody["assistant_id"] != c.Agents["drawing"] ||
		gotBody["type"] != "Intelligent Editing" || gotBody["rm_label_watermark"] != true {
		t.Errorf("edit body 关键字段错: %v", gotBody)
	}
	var res struct {
		ImageURL       string `json:"image_url"`
		ConversationID string `json:"conversation_id"`
	}
	if json.Unmarshal(raw, &res) != nil || res.ImageURL == "" || len(res.ConversationID) != 24 {
		t.Errorf("result 解析错: %s", raw)
	}
	b := EditBody("u", "把背景改成夜空", "", "")
	if !strings.HasPrefix(b["title"].(string), "【智能编辑】 ") {
		t.Errorf("title 缺省前缀错: %v", b["title"])
	}
}

// ── image_pipeline：拼版预算/图床回退与实现一致（契约 v3 新增）──
func TestContractImagePipeline(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var full struct {
		ImagePipeline struct {
			Command   string `json:"command"`
			Blocking  bool   `json:"blocking"`
			RefsCompo struct {
				BudgetPixels int    `json:"budget_pixels"`
				Format       string `json:"format"`
				Outline      string `json:"outline"`
				Layout       string `json:"layout"`
				Labels       string `json:"labels"`
				UnitGap      int    `json:"unit_gap"`
			} `json:"refs_composite"`
			PromptPrefix string `json:"prompt_prefix"`
			Upload       struct {
				Primary  string `json:"primary"`
				Fallback string `json:"fallback"`
				Body     string `json:"body"`
			} `json:"upload"`
			RmWatermark struct {
				Hardcoded bool   `json:"hardcoded"`
				Evidence  string `json:"evidence"`
			} `json:"rm_label_watermark"`
		} `json:"image_pipeline"`
	}
	if json.Unmarshal(raw, &full) != nil {
		t.Fatal("image_pipeline 解析失败")
	}
	p := full.ImagePipeline
	if p.Command != "image" || !p.Blocking {
		t.Errorf("image_pipeline.command/blocking 错: %+v", p)
	}
	if p.RefsCompo.BudgetPixels != 4800000 || p.RefsCompo.Format != "jpeg" ||
		!strings.Contains(p.RefsCompo.Outline, "方形") ||
		!strings.Contains(p.RefsCompo.Layout, "单行左右布局") ||
		!strings.Contains(p.RefsCompo.Labels, "框内") ||
		p.RefsCompo.UnitGap != 40 {
		t.Errorf("拼版契约与实现不一致: %+v", p.RefsCompo)
	}
	if p.PromptPrefix != EDIT_PROMPT_CONSTRAINT {
		t.Errorf("prompt_prefix 与 CLI 硬编码约束不一致: %q", p.PromptPrefix)
	}
	if !p.RmWatermark.Hardcoded || !strings.Contains(p.RmWatermark.Evidence, "实证") {
		t.Errorf("水印默认值契约错: %+v", p.RmWatermark)
	}
	if !strings.Contains(p.Upload.Primary, "meitu") || !strings.Contains(p.Upload.Fallback, "icve") ||
		!strings.Contains(p.Upload.Body, "raw binary") {
		t.Errorf("upload 契约描述与实现不符: %+v", p.Upload)
	}
}

func keysOfAny(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
