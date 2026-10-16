package glm

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSignMatchesCaptureEvidence 锁死签名公式（PROTOCOL §2 的复算证据 6/6 命中之一）。
func TestSignMatchesCaptureEvidence(t *testing.T) {
	const nonce, want = "75988225830f4d93b6702dcd0a2e9baf", "a32ee7a2876ad44e6658475f0a7c69eb"
	if got := Sign(1790732179905, nonce); got != want {
		t.Fatalf("Sign = %s，期望 %s", got, want)
	}
}

// TestHeadersShape 校验 IDE 指纹头集合：缺任一类都会被 WAF 挂起或 40012 [实证 PROTOCOL §2]。
func TestHeadersShape(t *testing.T) {
	h := Headers("T", "text/event-stream", "dev1")
	for _, k := range []string{"X-Sign", "X-Nonce", "X-Timestamp", "X-Device-Id", "X-Request-Id",
		"Authorization", "App-Name", "X-App-Platform", "X-App-Version", "X-Exp-Groups",
		"sec-ch-ua", "sec-ch-ua-platform", "Sec-Fetch-Dest", "Sec-Fetch-Site", "Origin", "User-Agent"} {
		if _, ok := h[k]; !ok {
			t.Errorf("缺少请求头 %s", k)
		}
	}
	if h["X-App-Platform"] != "win" || h["X-App-Version"] != "2.0.6" || h["App-Name"] != "chatglm" {
		t.Errorf("必须是 IDE(win/2.0.6) 指纹: %q %q", h["X-App-Platform"], h["X-App-Version"])
	}
	if len(h["X-Nonce"]) != 32 || len(h["X-Timestamp"]) != 13 || len(h["X-Sign"]) != 32 {
		t.Errorf("签名三件套长度异常: %q %q %q", h["X-Timestamp"], h["X-Nonce"], h["X-Sign"])
	}
	if !strings.Contains(h["User-Agent"], "Electron/") || !strings.Contains(h["User-Agent"], "dev1") {
		t.Errorf("UA 必须是 Electron 且内嵌真实 DeviceId: %q", h["User-Agent"])
	}
	// ⚠️ 关键：Headers 里绝不允许出现 Cookie（显式 Cookie 头会覆盖 jar ⇒ 持续 40012）
	if _, ok := h["Cookie"]; ok {
		t.Errorf("Headers 不得设置 Cookie")
	}
	if _, ok := Headers("", "", "")["Authorization"]; ok {
		t.Errorf("空 token 不该带 Authorization")
	}
	if _, ok := Headers("", "", "")["X-Device-Id"]; ok {
		t.Errorf("无 device_id 时不该带 X-Device-Id")
	}
}

// ── 凭据 ───────────────────────────────────────────────────
var fakePayload = map[string]any{"uid": "6982e0ad0efeac7998789a48",
	"device_id": "c37ee0587ac14b9483f419380d8d23ca", "type": "refresh", "exp": float64(4102444800)}

func fakeJWT(payload map[string]any) string {
	b := base64.RawURLEncoding.EncodeToString(mustJSON(payload))
	return "eyJhbGciOiJIUzI1NiJ9." + b + ".sig"
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// TestMakeCredentialFromCookie 覆盖「整条 cookie 粘贴」入口，并确认 WAF 指纹 cookie 被丢弃。
func TestMakeCredentialFromCookie(t *testing.T) {
	rt := fakeJWT(fakePayload)
	c := MakeCredential("", "", "", "", "ssxmod_itna=xxx; acw_tc=yyy; _c_WBKFRo=zz;"+
		" chatglm_refresh_token="+rt+"; chatglm_user_id=6982e0ad0efeac7998789a48")
	if c.RefreshToken != rt {
		t.Errorf("没解析出 refresh_token")
	}
	if c.UserID == "" {
		t.Errorf("user_id 应由 cookie 补齐")
	}
	if c.DeviceID != fakePayload["device_id"] {
		t.Errorf("device_id 应从 JWT 补齐，得到 %q", c.DeviceID)
	}
	for _, k := range []string{"acw_tc", "ssxmod_itna", "_c_WBKFRo", "chatglm_refresh_token"} {
		if _, dup := c.ExtraCookies[k]; dup {
			t.Errorf("%s 不该留在 extra_cookies 里", k)
		}
	}
}

// TestAccessExpired 覆盖「access 缺失/临期都要刷新」。
func TestAccessExpired(t *testing.T) {
	c := MakeCredential(fakeJWT(fakePayload), "", "", "", "")
	if !c.AccessExpired() {
		t.Errorf("没有 access_token 必须视为过期")
	}
	now := time.Now().Unix()
	c.AccessToken = fakeJWT(map[string]any{"exp": float64(now + 60)})
	if !c.AccessExpired() {
		t.Errorf("距到期不足 AUTH_SKEW(%d 秒) 应提前刷新", AUTH_SKEW)
	}
	c.AccessToken = fakeJWT(map[string]any{"exp": float64(now + 3600)})
	if c.AccessExpired() {
		t.Errorf("远期到期不该刷新")
	}
	if c.RefreshExp() != int64(fakePayload["exp"].(float64)) {
		t.Errorf("refresh_exp = %d", c.RefreshExp())
	}
}

// TestSaveLoadRoundtrip 覆盖轮换后能正确读回，且**未知键不丢**（与 Python 侧共用同一文件）。
func TestSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "glm_credential.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"old","refer_991":"keep-me","note":{"a":1}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if c.origin != "old" {
		t.Errorf("origin 应记录本次刷新依据，得到 %q", c.origin)
	}
	c.AccessToken, c.RefreshToken = fakeJWT(fakePayload), fakeJWT(fakePayload)
	if err := c.Save(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	again, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.RefreshToken != c.RefreshToken || again.AccessToken != c.AccessToken {
		t.Errorf("回读不一致")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "refer_991") || !strings.Contains(string(b), "note") {
		t.Errorf("未知键必须保留，得到 %s", b)
	}
}

// TestSaveRefusesClobber 覆盖多进程并发：磁盘上的 refresh 已被别的进程轮换 ⇒ 拒绝覆盖。
func TestSaveRefusesClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"A"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	c.RefreshToken = "B" // 我们用 A 换到 B，但另一个进程已写入 C
	if err := os.WriteFile(path, []byte(`{"refresh_token":"C"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err == nil {
		t.Fatalf("必须拒绝覆盖别的进程写回的凭据")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "\"C\"") {
		t.Errorf("磁盘内容不该被动，得到 %s", b)
	}
}

// TestLoadBareTokenFile 覆盖「文件里只有一行裸 refresh token」。
func TestLoadBareTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.txt")
	rt := fakeJWT(fakePayload)
	if err := os.WriteFile(path, []byte(rt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != rt || c.UserID == "" {
		t.Errorf("裸 token 文件应可用，得到 %+v", c)
	}
}

// TestNoCredentialFailsEarly 覆盖 fail-early：既没文件也没环境变量 ⇒ 明确报错而不是静默访客。
func TestNoCredentialFailsEarly(t *testing.T) {
	home := t.TempDir()
	t.Setenv(ENV_CREDENTIAL_FILE, "")
	t.Setenv(ENV_REFRESH_TOKEN, "")
	t.Setenv("HOME", home)        // unix 下的 ~
	t.Setenv("USERPROFILE", home) // Windows 下的 ~
	if _, err := LoadCredential(""); err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("应报「缺少 refresh_token」，得到 %v", err)
	}
}
