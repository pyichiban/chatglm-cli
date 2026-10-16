package glm

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Sign 计算 X-Sign = md5("<ts>-<nonce>-<SIGN_SECRET>")，小写 hex。
// 复算证据（PROTOCOL §2，6/6 命中）：
//
//	md5("1790732179905-75988225830f4d93b6702dcd0a2e9baf-8a1317a7468aa3ad86e997d08f3f31cb")
//	= a32ee7a2876ad44e6658475f0a7c69eb
func Sign(ts int64, nonce string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%d-%s-%s", ts, nonce, SIGN_SECRET)))
	return hex.EncodeToString(sum[:])
}

// Nonce 返回 32 位小写 hex 随机串（对齐 Python secrets.token_hex(16)）。
func Nonce() string { return randHex(16) }

func randHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return hex.EncodeToString(b)
}

// Headers 组装一次请求的完整头（**不含 Cookie**：Cookie 一律交给 cookie jar，
// 显式 Cookie 头会覆盖 jar，使服务端下发的 WAF cookie 无法带回 → 持续 40012）。
// 头集合必须与 IDE 客户端逐头一致，缺任一类会被 WAF 挂起或 40012 [实证 PROTOCOL §2]。
func Headers(token, accept, deviceID string) map[string]string {
	ts := time.Now().UnixMilli()
	nonce := Nonce()
	ua := strings.Replace(USER_AGENT, "{device_id}", deviceID, 1)
	if deviceID == "" {
		ua = strings.Replace(ua, "DeviceId/{device_id}", "DeviceId/", 1)
	}
	h := map[string]string{
		// 基础
		"Accept": accept,
		// ⚠️ 与 Python 侧（"gzip, deflate, br, zstd"）的唯一差异：Go stdlib 只能自己解 gzip/deflate，
		//    为零依赖不发 br/zstd。实测上游并不压缩 SSE，故不影响（见 README.md 指纹差异表）。
		"Accept-Encoding": "gzip, deflate",
		"Accept-Language": "zh-CN",
		"App-Name":        APP_NAME,
		"Origin":          "https://chatglm.cn",
		"User-Agent":      ua,
		// 业务伪装头
		"X-App-Platform": APP_PLATFORM,
		"X-App-Version":  APP_VERSION,
		"X-App-fr":       APP_FR,
		"X-Lang":         "zh",
		"X-Device-Model": "",
		"X-Device-Brand": "",
		"X-Exp-Groups":   EXP_GROUPS,
		// 浏览器客户端提示（IDE 是 Electron/Chromium 150）
		"sec-ch-ua":          SEC_CH_UA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		// 签名三件套
		"X-Timestamp": fmt.Sprintf("%d", ts),
		"X-Nonce":     nonce,
		"X-Sign":      Sign(ts, nonce),
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	if deviceID != "" {
		h["X-Device-Id"] = deviceID // 与 JWT.device_id 一致 [实证 PROTOCOL §1.2]
	}
	h["X-Request-Id"] = randHex(16)
	return h
}
