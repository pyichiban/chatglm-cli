// Package cli 是 glm2cli 的参数契约层：解析命令 → 调协议层 → 输出 JSONL 事件。
//
// 契约纪律（与 docs/glm2cli设计.md 一致）：
//   - stdout 只有一条条 JSONL 数据，日志全部走 stderr；
//   - 永不交互（不 prompt、不读 tty）；失败以 {"type":"error"} 事件 + 非零退出码表达；
//   - 所有 flag 注册在同一把 FlagSet 上（见 Flags），因此 **flag 可写在任意位置**，
//     位置参数（命令名、prompt、conv 子命令）一律靠「非 flag token」识别；
//   - 无本地状态：唯一落盘的是凭据文件（refresh_token 会轮换，不落盘 = 账号失效）。
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pyichiban/chatglm-cli/internal/glm"
)

const VERSION = "0.1.0"

// Flags 是全部命令行参数（一张扁平表）。命令只读自己关心的字段，
// 好处：flag 顺序无关（`--cid X chat` 与 `chat --cid X` 等价），未知 flag 直接报错。
type Flags struct {
	Cred, Base              string
	Timeout                 int
	DryRun, Debug, Insecure bool

	Prompt      string
	CID         string
	Agent       string
	Model       string
	Mode        string
	Effort      string
	Web         bool
	Ratio       string
	Style       string
	Scene       string
	Title       string
	TitleFile   string
	RMWatermark bool

	// raw：逆向逃生门
	Method, Path, Body string

	Grep   string
	Yes    bool
	Raw    bool
	Params kvValue

	// 内部：解析后的位置参数（[0] 是命令名）
	rest []string
}

func (f *Flags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.Cred, "cred", "", "凭据文件（默认 $GLM_CREDENTIAL_FILE → ./glm_credential.json → ~/.glm2cli/credential.json）")
	fs.StringVar(&f.Base, "base", glm.BASE, "上游基址")
	fs.IntVar(&f.Timeout, "timeout", glm.READ_TIMEOUT, "非流式请求超时（秒）；流式不受此限制")
	fs.BoolVar(&f.DryRun, "dry-run", false, "只打印将发送的请求，不发网络")
	fs.BoolVar(&f.Debug, "v", false, "详细日志到 stderr")
	fs.BoolVar(&f.Insecure, "insecure", false, "跳过 TLS 校验（仅配合本地抓包调试）")

	fs.StringVar(&f.Prompt, "p", "", "提示词；'-' 表示从 stdin 读")
	fs.StringVar(&f.Prompt, "prompt", "", "")
	fs.StringVar(&f.CID, "cid", "", "conversation_id（24 位小写 hex）；省略=新会话。必须显式传入")
	fs.StringVar(&f.Agent, "agent", "main", "main|drawing|read|video|study 或裸 assistant_id")
	fs.StringVar(&f.Model, "model", glm.DEFAULT_MODEL, "selected_model")
	fs.StringVar(&f.Mode, "mode", "", "chat_mode 原样透传（实测抓包为 thinking）")
	fs.StringVar(&f.Effort, "effort", "", "reasoning_effort 原样透传（深度 thinking / 极致 deep_thinking）")
	fs.BoolVar(&f.Web, "web", true, "联网检索；--web=false 关闭")
	fs.StringVar(&f.Ratio, "ratio", "", "生图比例 1:1|3:4|4:3|9:16|16:9（给了就自动切画图 agent）")
	fs.StringVar(&f.Style, "style", "", "生图风格（实测已失效，仅透传）")
	fs.StringVar(&f.Scene, "scene", "", "生图场景（实测已失效，仅透传）")
	fs.StringVar(&f.Title, "title", "", "会话标题：chat 给新会话改名；conv rename 的新标题")
	fs.StringVar(&f.TitleFile, "title-file", "", "从文件读标题（避开 shell 引号）")
	fs.BoolVar(&f.RMWatermark, "rm-watermark", false, "cogview.rm_label_watermark（是否真去水印 [未知]）")

	fs.StringVar(&f.Grep, "grep", "", "按标题子串过滤")
	fs.BoolVar(&f.Yes, "yes", false, "危险操作（delete）的显式确认")
	fs.BoolVar(&f.Raw, "raw", false, "输出上游 result 原样，不做字段提取")
	fs.Var(&f.Params, "param", "追加 query 参数 k=v（conv history 试验未实证的翻页参数名，可重复）")
	fs.StringVar(&f.Method, "method", "GET", "raw: HTTP 方法 GET|POST")
	fs.StringVar(&f.Path, "path", "", "raw: 以 /chatglm/ 开头的端点路径")
	fs.StringVar(&f.Body, "body", "", "raw: JSON 请求体（'-' 从 stdin 读）")
}

// kvValue 是重复型 flag（history 实试验证未实证的翻页参数）。
type kvValue map[string]string

func (k *kvValue) String() string { return fmt.Sprint(map[string]string(*k)) }
func (k *kvValue) Set(v string) error {
	key, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--param 需要 k=v，收到 %q", v)
	}
	if *k == nil {
		*k = kvValue{}
	}
	(*k)[key] = val
	return nil
}

// ── App：一次进程生命周期 ──────────────────────────────────
type App struct {
	enc    *json.Encoder
	c      *glm.Client
	cred   *glm.Credential
	f      *Flags
	start  time.Time
	debug  bool
	dryRun bool
}

// Cmd 是一个子命令（usage 与 schema 都从这里生成，避免新增命令漏文档）。
type Cmd struct {
	Name, Usage string
	Run         func(a *App) error
	Offline     bool // 不需要凭据/网络（help、schema）
}

func commands() []Cmd {
	return []Cmd{
		{Name: "chat", Usage: "流式对话：[-p 提示词|--cid ID] [--agent main|drawing] [--model M] [--mode M] [--effort E] [--web=false] [--ratio R] [--title T] [提示词]", Run: runChat},
		{Name: "user", Usage: "当前用户信息（含积分余额）；--raw 输出上游原样", Run: runUser},
		{Name: "models", Usage: "可用模型与 reasoning_effort 枚举", Run: runModels},
		{Name: "agents", Usage: "内置智能体（assistant_id）表", Run: runAgents},
		{Name: "auth", Usage: "凭据：glm2cli auth [check|refresh]（refresh 会轮换并落盘）", Run: runAuth},
		{Name: "conv", Usage: "会话管理：list|show|history|rename|title|delete|stop|read（见 glm2cli help conv）", Run: runConv},
		{Name: "raw", Usage: "逃生门：直打任意 /chatglm/ 端点（--method --path --param --body），用于补实证", Run: runRaw},
		{Name: "schema", Usage: "输出命令/事件/退出码的机器可读契约", Run: runSchema, Offline: true},
		{Name: "help", Usage: "用法；glm2cli help conv 看会话子命令", Run: runHelp, Offline: true},
	}
}

// Main 是进程入口：返回退出码。
func Main(argv []string) int {
	for _, a := range argv {
		if a == "--version" || a == "-V" {
			argv = []string{"help", "version"}
			break
		}
	}
	fs, f := newFlagSet("glm2cli")
	name, err := parse(fs, argv, f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		usage(os.Stderr)
		return 2
	}
	if name == "" {
		usage(os.Stderr)
		return 2
	}
	for _, c := range commands() {
		if c.Name == name {
			return exec(c, f)
		}
	}
	fmt.Fprintf(os.Stderr, "未知命令: %s\n", name)
	usage(os.Stderr)
	return 2
}

func newFlagSet(name string) (*flag.FlagSet, *Flags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 错误统一由我们格式化输出（stderr）
	f := &Flags{}
	f.register(fs)
	return fs, f
}

// parse 用「已知 flag 表」把 argv 拆成 flag 项与位置参数：flag 可写在任意位置，
// 第一个非 flag token 是命令名，其余按原序进 f.rest。未知 flag 直接报错（fail early）。
func parse(fs *flag.FlagSet, argv []string, f *Flags) (string, error) {
	var items, pos []string
	for i := 0; i < len(argv); i++ {
		t := argv[i]
		if t == "-" || !strings.HasPrefix(t, "-") || len(t) == 1 {
			pos = append(pos, t)
			continue
		}
		name, _, hasVal := strings.Cut(strings.TrimLeft(t, "-"), "=")
		fl := fs.Lookup(name)
		if fl == nil {
			return "", fmt.Errorf("未知参数: %s", t)
		}
		items = append(items, t)
		if hasVal || isBoolFlag(fl) {
			continue
		}
		if i+1 >= len(argv) {
			return "", fmt.Errorf("%s 缺少取值", t)
		}
		i++
		items = items[:len(items)-1] // 去掉刚放的裸 flag，改存 --flag=value，避免取值被当成位置参数
		items = append(items, t+"="+argv[i])
	}
	if err := fs.Parse(items); err != nil {
		return "", err
	}
	f.rest = pos
	if len(pos) == 0 {
		return "", nil
	}
	return pos[0], nil
}

func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func exec(c Cmd, f *Flags) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false) // 正文里的 < > & 不能被转义，否则代码/Markdown 被改坏
	a := &App{enc: enc, f: f, start: time.Now(), debug: f.Debug, dryRun: f.DryRun}

	if c.Offline {
		if err := c.Run(a); err != nil {
			a.fail(err)
			return code(err)
		}
		return 0
	}
	// --dry-run 不碰凭据、不发网络（chat/conv/raw 会自己打印请求体，其余命令给一行就够）
	if f.DryRun {
		if c.Name == "chat" || c.Name == "conv" || c.Name == "raw" {
			if err := c.Run(a); err != nil {
				a.fail(err)
				return code(err)
			}
			return 0
		}
		a.result("dry-run", map[string]any{"cmd": c.Name, "args": f.rest, "base": f.Base})
		return 0
	}
	cred, err := glm.LoadCredential(f.Cred)
	if err != nil {
		a.fail(err)
		return code(err)
	}
	cl, err := glm.NewClient(ctx, cred, glm.Opt{Base: f.Base, Insecure: f.Insecure,
		Timeout: time.Duration(f.Timeout) * time.Second, Debug: f.Debug})
	if err != nil {
		a.fail(err)
		return code(err)
	}
	cl.Warn = func(msg string) { a.ev(&glm.Event{Type: "warn", Code: "credential", Message: msg}) }
	a.c, a.cred = cl, cred
	if err := c.Run(a); err != nil {
		a.fail(err)
		return code(err)
	}
	return 0
}

func code(err error) int {
	if ge, ok := err.(*glm.Error); ok {
		return int(ge.Code)
	}
	return int(glm.CUpstream)
}

// args 返回命令名之后的位置参数。
func (a *App) args() []string { return a.f.rest[1:] }

// ── 输出 ───────────────────────────────────────────────────
func (a *App) ev(e *glm.Event) {
	e.V = glm.EV
	if e.RequestID == "" && a.c != nil {
		e.RequestID = a.c.LastRID
	}
	if err := a.enc.Encode(e); err != nil {
		fmt.Fprintf(os.Stderr, "写 stdout 失败: %v\n", err)
	}
}

func (a *App) result(cmd string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		a.fail(glm.NewErr(glm.CUpstream, "序列化结果失败: %v", err))
		return
	}
	a.ev(&glm.Event{Type: "result", Cmd: cmd, Data: b})
}

// passthrough 输出上游 result（原样 JSON，不做字段假设）。
func (a *App) passthrough(cmd string, raw json.RawMessage) {
	a.ev(&glm.Event{Type: "result", Cmd: cmd, Data: raw})
}

func (a *App) fail(err error) {
	a.ev((&glm.Event{}).Err(err))
	if a.debug {
		fmt.Fprintf(os.Stderr, "debug: %v\n", err)
	}
}

func (a *App) logf(f string, x ...any) {
	if a.debug {
		fmt.Fprintf(os.Stderr, "glm2cli: "+f+"\n", x...)
	}
}

// dryCall 统一写类命令的 --dry-run：只打印将要发的请求，不发网络（dry-run 下 a.c 为 nil）。
func (a *App) dryCall(cmd, method, path string, body any) bool {
	if !a.dryRun {
		return false
	}
	base := a.f.Base
	if a.c != nil {
		base = a.c.Base
	}
	a.result("dry-run", map[string]any{"cmd": cmd, "method": method, "url": base + path, "body": body})
	return true
}

// ── chat ───────────────────────────────────────────────────
func runChat(a *App) error {
	f := a.f
	prompt, err := readPrompt(f.Prompt, a.args())
	if err != nil {
		return err
	}
	agent, err := resolveAgent(f.Agent, f.Ratio)
	if err != nil {
		return err
	}
	if f.CID != "" && !glm.CIDPattern.MatchString(f.CID) {
		return glm.NewErr(glm.CUsage, "--cid 必须是 24 位小写 hex（上游 conversation_id），收到 %q", f.CID)
	}
	if f.Ratio != "" && !validRatio(f.Ratio) {
		return glm.NewErr(glm.CUsage, "--ratio %q 不在实测有效集合（上游会静默降级为 1:1）: 1:1|3:4|4:3|9:16|16:9", f.Ratio)
	}
	opt := glm.ChatOpt{Agent: agent, CID: f.CID, Prompt: prompt, Model: f.Model, Mode: f.Mode,
		Effort: f.Effort, Web: f.Web, Ratio: f.Ratio, Style: f.Style, Scene: f.Scene,
		RMWatermark: f.RMWatermark}
	if a.dryCall("chat", "POST", glm.EP_CHAT, glm.ChatBody(opt)) {
		return nil
	}

	resp, err := a.c.Chat(opt)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	newConv := f.CID == ""
	var metaSent bool
	agg, rerr := glm.ReadStream(resp.Body, func(e *glm.Event) { a.ev(e) }, func(g *glm.Agg) {
		if metaSent {
			return
		}
		metaSent = true // 拿到首帧再发 meta ⇒ meta 里的 conversation_id 一定是真实值（首轮由服务端懒创建）
		a.ev(&glm.Event{Type: "meta", Cmd: "chat", ConversationID: g.ConversationID, MessageID: g.MessageID,
			AgentID: agent, Model: f.Model, New: newConv, TS: a.start.Unix()})
	})
	a.ev(&glm.Event{Type: "done", Cmd: "chat", ConversationID: agg.ConversationID, MessageID: agg.MessageID,
		AgentID: agg.AgentID, Model: agg.Model, Frames: agg.Frames, New: newConv,
		ElapsedMS: time.Since(a.start).Milliseconds(), Finished: agg.Finished, Images: agg.Images,
		TextChars: len([]rune(agg.Text())), ReasoningChars: len([]rune(agg.Reasoning()))})

	if agg.Err != nil { // 上游用 error 帧收尾：done 已给统计，再以 error 表达原因
		return glm.NewErr(glm.CUpstream, "上游生成失败 error_code=%d: %s", agg.Err.ErrorCode, agg.Err.ErrMsg)
	}
	if rerr != nil {
		a.stopBestEffort(agg.MessageID)
		return rerr
	}
	if f.Title != "" && agg.ConversationID != "" {
		if _, err := a.c.Rename(agg.ConversationID, f.Title); err != nil {
			return err
		}
	}
	return nil
}

// stopBestEffort 在流被截断/被 Ctrl-C 后尽力中止服务端生成（失败就忽略，不掩盖主错误）。
func (a *App) stopBestEffort(mid string) {
	if mid == "" || !glm.CIDPattern.MatchString(mid) {
		return
	}
	a.logf("流未正常收尾，尝试 stop_stream(%s)", mid)
	_, _ = a.c.WithContext(context.Background()).StopStream(mid)
}

// readPrompt 优先 --prompt/-p，其次位置参数，最后 stdin（非 TTY 时自动读）。
func readPrompt(flagVal string, rest []string) (string, error) {
	if flagVal == "-" {
		return readStdin()
	}
	if flagVal != "" {
		return flagVal, nil
	}
	if len(rest) > 0 && rest[0] != "-" {
		return strings.Join(rest, " "), nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return "", glm.NewErr(glm.CUsage, "没有提示词：用 --prompt/-p、位置参数，或从 stdin 输入")
	}
	return readStdin()
}

func readStdin() (string, error) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return "", glm.NewErr(glm.CUsage, "读 stdin 失败: %v", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", glm.NewErr(glm.CUsage, "stdin 是空的")
	}
	return s, nil
}

// resolveAgent 把别名展开成 assistant_id；给了 --ratio 时默认切到画图 agent。
func resolveAgent(alias, ratio string) (string, error) {
	m := map[string]string{"main": glm.DEFAULT_ASSISTANT, "drawing": glm.ASSISTANT_DRAWING,
		"read": glm.ASSISTANT_READ, "video": glm.ASSISTANT_VIDEO, "study": glm.ASSISTANT_STUDY}
	if ratio != "" && (alias == "" || alias == "main") {
		return glm.ASSISTANT_DRAWING, nil // --ratio 是生图的显式信号
	}
	if id, ok := m[alias]; ok {
		return id, nil
	}
	if alias == "" || glm.CIDPattern.MatchString(alias) {
		return alias, nil
	}
	return "", glm.NewErr(glm.CUsage, "--agent 未知: %q（可用 main|drawing|read|video|study 或 24 位 hex）", alias)
}

// validRatio 是 2026-10-15 实测有效集合（4:3 不在前端预设但上游接受；21:9 等被静默降级 1:1）。
func validRatio(r string) bool {
	for _, v := range []string{"1:1", "3:4", "4:3", "9:16", "16:9"} {
		if v == r {
			return true
		}
	}
	return false
}

// ── 只读类命令 ─────────────────────────────────────────────
func runUser(a *App) error {
	res, err := a.c.UserInfo()
	if err != nil {
		return err
	}
	if a.f.Raw {
		a.passthrough("user", res)
		return nil
	}
	var info map[string]any
	_ = json.Unmarshal(res, &info)
	mi, _ := info["member_info"].(map[string]any)
	uid := a.cred.UserID
	if s, ok := info["_id"].(string); ok {
		uid = s
	}
	a.result("user", map[string]any{"uid": uid, "nickname": info["nickname"], "avatar": info["avatar"],
		"is_guest": info["is_guest"], "phone": info["phone"], "left_score": mi["left_score"],
		"member_info": mi})
	return nil
}

// creditsOf 已废弃（V13 定稿：目前不消耗积分，余额原样返回不折算；保留空实现防误用）。
func creditsOf(mi map[string]any) map[string]any {
	return map[string]any{"left_score": mi["left_score"]}
}

func runModels(a *App) error {
	res, err := a.c.Models()
	if err != nil {
		return err
	}
	a.passthrough("models", res)
	return nil
}

func runAgents(a *App) error {
	a.result("agents", glm.AGENTS())
	return nil
}

func runAuth(a *App) error {
	sub := "check"
	if args := a.args(); len(args) > 0 {
		sub = args[0]
	}
	view := map[string]any{"source": a.cred.Source, "user_id": a.cred.UserID,
		"device_id": a.cred.DeviceID, "access_exp": a.cred.AccessExp(),
		"refresh_exp": a.cred.RefreshExp(), "access_expired": a.cred.AccessExpired(),
		"refresh_token": mask(a.cred.RefreshToken), "access_token": mask(a.cred.AccessToken),
		"can_persist": a.cred.Source != "" && a.cred.Source != "env"}
	if sub == "check" {
		a.result("auth.check", view)
		return nil
	}
	if sub != "refresh" {
		return glm.NewErr(glm.CUsage, "auth 只支持 check|refresh")
	}
	if a.dryCall("auth.refresh", "POST", glm.EP_REFRESH, "{}") {
		return nil
	}
	if err := a.c.EnsureToken(true); err != nil {
		return err
	}
	view["access_exp"], view["access_expired"] = a.cred.AccessExp(), a.cred.AccessExpired()
	view["refresh_token"] = mask(a.cred.RefreshToken) // 轮换后的新根凭据
	view["saved_to"] = a.cred.Source
	a.result("auth.refresh", view)
	return nil
}

func mask(tok string) string {
	if tok == "" {
		return "(empty)"
	}
	return fmt.Sprintf("%.8s…(len=%d)", tok, len(tok))
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `glm2cli %s —— 智谱清言(chatglm.cn) 私有接口的命令行客户端（仅 IDE 通道，纯流式 JSONL）

用法: glm2cli <命令> [flag] [位置参数]      flag 可写在任意位置
      glm2cli help conv                     看会话子命令
      glm2cli schema                        机器可读契约（事件/退出码/命令）

`, VERSION)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-7s %s\n", c.Name, c.Usage)
	}
	fmt.Fprintf(w, `
凭据（按优先级）: --cred > $GLM_CREDENTIAL_FILE > ./glm_credential.json > ~/.glm2cli/credential.json
                  > 环境变量 $GLM_REFRESH_TOKEN（env 模式无法落盘轮换后的 refresh_token）
取凭据: 浏览器登录 chatglm.cn → F12 → Application → Cookies → 复制 chatglm_refresh_token 值

输出: stdout 每行一个 JSON 事件（meta/text/reasoning/image/item/result/done/warn/error），日志走 stderr
`)
}

func goVersion() string { return runtime.Version() }
