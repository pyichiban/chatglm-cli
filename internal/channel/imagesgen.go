package channel

// images.generate —— PLUGIN_PROTOCOL §5.8.2 契约的 glm 实现（控制面：不发事件帧，只写一帧 result）。
//
// 两条路径：
//   - 无参考图 → cogview 文生图（ASSISTANT_DRAWING 对话通道，rm_label_watermark 写死去水印）；
//     n>1 时串行多次请求（间隔 2s，风控纪律），上限 4。
//   - 有参考图 → 客户端拼版（imgutil.BuildRefSheet 单行左右布局+方形外框+referenceN 标注）
//     → file.sang.pub 托管 → drawing-api/v1/image/reference 智能编辑（输出比例跟随输入图）。
//     参考图字节合计 ≤24MB。
//
// W3-07：参考图/图片 base64 绝不落日志，日志只允许张数/字节数/mime。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig 注册格式
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyichiban/chatglm-cli/internal/glm"
	"github.com/pyichiban/chatglm-cli/internal/imgutil"
	"github.com/pyichiban/chatglm-cli/internal/protocol/jsonl"
	"github.com/pyichiban/chatglm-cli/internal/protocol/stat"
)


const maxRefBytes = 24 << 20 // 参考图字节合计上限
const maxImages = 4          // cogview 单次一图；n 上限（防风控滥用）

type imageRef struct {
	B64JSON string `json:"b64_json"`
	MimeType string `json:"mime_type"`
	URL      string `json:"url"`
}

type imagesParams struct {
	Prompt         string     `json:"prompt"`
	N              int        `json:"n"`
	Size           string     `json:"size"`          // "WxH" | "auto"
	AspectRatio    string     `json:"aspect_ratio"`  // "w:h"，优先于 size
	ResponseFormat string     `json:"response_format"` // b64_json|url，缺省 b64_json
	Images         []imageRef `json:"images"`
	UID            string     `json:"uid"`
	Config         json.RawMessage `json:"config"`
}

// validImageRatio 是 2026-10-15 实测有效集合（与内核 --ratio 同源）。
func validImageRatio(r string) bool {
	for _, v := range []string{"1:1", "3:4", "4:3", "9:16", "16:9"} {
		if v == r {
			return true
		}
	}
	return false
}

// ResolveImageRatio 折算 aspect_ratio/size 为 glm ratio：aspect_ratio 优先于 size [契约]；
// size "auto"→""（内核默认 1:1）；"WxH" 约分后必须在有效集合，否则 bad_params（契约：本地严格校验）。
func ResolveImageRatio(aspectRatio, size string) (string, *jsonl.ErrObj) {
	if aspectRatio != "" {
		if !validImageRatio(aspectRatio) {
			return "", errf("bad_params", "request", "aspect_ratio %q 不支持（可用 1:1|3:4|4:3|9:16|16:9）", aspectRatio)
		}
		return aspectRatio, nil
	}
	size = strings.TrimSpace(size)
	if size == "" || size == "auto" {
		return "", nil
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return "", errf("bad_params", "request", "size %q 不是 WxH 形式", size)
	}
	a, b := w, h
	for b != 0 { // 约分
		a, b = b, a%b
	}
	r := fmt.Sprintf("%d:%d", w/a, h/a)
	if !validImageRatio(r) {
		return "", errf("bad_params", "request", "size %q 折算比例 %s 不支持（可用 1:1|3:4|4:3|9:16|16:9）", size, r)
	}
	return r, nil
}

var imgHTTPClient = &http.Client{Timeout: 120 * time.Second}

// fetchRef 把一帧参考图落成临时文件（url 下载 / b64_json 解码），返回路径与字节数。
func fetchRef(dir string, i int, r imageRef, budget *int) (string, *jsonl.ErrObj) {
	var data []byte
	switch {
	case r.URL != "":
		resp, err := imgHTTPClient.Get(r.URL)
		if err != nil {
			return "", errf("image_invalid", "request", "参考图 %d 下载失败: %v", i+1, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", errf("image_invalid", "request", "参考图 %d 下载 HTTP %d", i+1, resp.StatusCode)
		}
		data, _ = io.ReadAll(io.LimitReader(resp.Body, maxRefBytes+1))
	case r.B64JSON != "":
		d, err := base64.StdEncoding.DecodeString(r.B64JSON)
		if err != nil {
			return "", errf("image_invalid", "request", "参考图 %d base64 解码失败: %v", i+1, err)
		}
		data = d
	default:
		return "", errf("bad_params", "request", "参考图 %d 缺少 b64_json/url", i+1)
	}
	if *budget+len(data) > maxRefBytes {
		return "", errf("image_invalid", "request", "参考图字节合计超过 24MB 上限（当前 %d）", *budget+len(data))
	}
	*budget += len(data)
	ext := mimeExt(r.MimeType, data)
	p := filepath.Join(dir, fmt.Sprintf("ref%d%s", i+1, ext))
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", errf("server", "channel", "参考图 %d 落盘失败: %v", i+1, err)
	}
	return p, nil
}

// mimeExt 由 mime 推扩展名，推不出看魔数，再推不出报 bad_params（契约：不猜）。
func mimeExt(mime string, data []byte) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "bmp"):
		return ".bmp"
	}
	if len(data) >= 8 {
		switch {
		case data[0] == 0x89 && data[1] == 'P':
			return ".png"
		case data[0] == 0xFF && data[1] == 0xD8:
			return ".jpg"
		case string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
			return ".webp"
		case string(data[:3]) == "GIF":
			return ".gif"
		case data[0] == 'B' && data[1] == 'M':
			return ".bmp"
		}
	}
	return "" // 调用方负责报错
}

// genCogview 走画图 agent 对话通道文生图，返回 (url, polished_prompt)。
func genCogview(ctx context.Context, ch *Channel, c *glm.Client, prompt, ratio string) (string, string, error) {
	resp, err := c.Chat(glm.ChatOpt{Agent: glm.ASSISTANT_DRAWING, Prompt: prompt, Ratio: ratio})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	url, polished := "", ""
	_, rerr := glm.ReadStream(resp.Body, func(ev *glm.Event) {
		switch ev.Type {
		case "image":
			url, polished = ev.URL, ev.PolishedPrompt
		}
	}, nil)
	if rerr != nil {
		return "", "", rerr
	}
	return url, polished, nil
}

// ImagesGenerate 处理 images.generate。
func (ch *Channel) ImagesGenerate(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	start := time.Now()
	var p imagesParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errf("bad_params", "request", "images.generate 参数解析失败: %v", err)
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return nil, errf("bad_params", "request", "prompt 必填")
	}
	if p.N <= 0 {
		p.N = 1
	}
	if p.N > maxImages {
		return nil, errf("bad_params", "request", "n 上限 %d（glm 单次一图，防风控）", maxImages)
	}
	format := p.ResponseFormat
	if format == "" {
		format = "b64_json" // 契约缺省：直链会过期
	}
	if format != "b64_json" && format != "url" {
		return nil, errf("bad_params", "request", "response_format 只支持 b64_json|url")
	}
	ratio, e := ResolveImageRatio(p.AspectRatio, p.Size)
	if e != nil {
		return nil, e
	}
	c, err := ch.Client(ctx, p.UID, p.Config)
	if err != nil {
		return nil, mapClientErr(err)
	}

	note := ""
	var produced []struct{ url, polished string }
	tmp, err := os.MkdirTemp("", "glm2cli-img")
	if err != nil {
		return nil, errf("server", "channel", "临时目录创建失败: %v", err)
	}
	defer os.RemoveAll(tmp)

	if len(p.Images) == 0 { // ── 文生图：串行 n 次 ──
		for i := 0; i < p.N; i++ {
			if i > 0 {
				time.Sleep(2 * time.Second) // 风控纪律：请求间隔
			}
			u, pp, err := genCogview(ctx, ch, c, p.Prompt, ratio)
			if err != nil {
				return nil, mapClientErr(err)
			}
			produced = append(produced, struct{ url, polished string }{u, pp})
		}
	} else { // ── 参考图生图：拼版（含单图：外框+标注链路一致）→ 托管 → 智能编辑（单图输出） ──
		budget := 0
		paths := make([]string, 0, len(p.Images))
		for i, r := range p.Images {
			path, e := fetchRef(tmp, i, r, &budget)
			if e != nil {
				return nil, e
			}
			paths = append(paths, path)
		}
		sheet, w, h, err := imgutil.BuildRefSheet(paths)
		if err != nil {
			return nil, errf("image_invalid", "request", "参考图拼版失败: %v", err)
		}
		sheetURL, _, err := imgutil.UploadJPEG(sheet)
		if err != nil {
			return nil, errf("server", "channel", "拼版图上传失败: %v", err)
		}
		res, err := c.EditImage(sheetURL, glm.EDIT_PROMPT_CONSTRAINT+"\n\n"+p.Prompt, "", ratio)
		if err != nil {
			return nil, mapClientErr(err)
		}
		outURL := extractString(res, "image_url")
		if outURL == "" {
			return nil, errf("server", "channel", "智能编辑未返回 image_url: %s", truncJSON(res))
		}
		produced = append(produced, struct{ url, polished string }{outURL, ""})
		note = fmt.Sprintf("%d 张参考图拼版(%dx%d)+智能编辑；输出比例跟随拼版图", len(paths), w, h)
	}

	// ── 组装 result data[] ──
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(produced))
	for i, it := range produced {
		entry := map[string]any{"revised_prompt": it.polished}
		if format == "url" {
			entry["url"] = it.url
		} else {
			raw, mime, w, h, e := downloadImage(it.url)
			if e != nil {
				return nil, e
			}
			entry["b64_json"] = base64.StdEncoding.EncodeToString(raw)
			entry["mime_type"], entry["width"], entry["height"] = mime, w, h
		}
		entry["index"] = i
		data = append(data, entry)
	}
	rec := stat.Record{Kind: "image", Account: p.UID, OK: true,
		TimeMs: int(time.Since(start).Milliseconds()), PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1, CachedTokens: -1}
	if ratio != "" {
		rec.Model = "glm/cogview@" + ratio
	}
	ch.Record(rec)
	out := map[string]any{"created": created, "data": data,
		"aspect_ratio": ratioOr(ratio), "usage": nil}
	if note != "" {
		out["note"] = note
	}
	return mustJSON(out), nil
}

// downloadImage 拉产物图并探测 mime 与尺寸（魔数判 jpeg/png，尺寸用 image.DecodeConfig）。
func downloadImage(url string) ([]byte, string, int, int, *jsonl.ErrObj) {
	resp, err := imgHTTPClient.Get(url)
	if err != nil {
		return nil, "", 0, 0, errf("server", "channel", "产物图下载失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if len(raw) < 8 {
		return nil, "", 0, 0, errf("server", "channel", "产物图过小或为空（%d 字节，可能被 WAF 拦截）", len(raw))
	}
	mime, w, h := sniffImage(raw)
	if mime == "" {
		return nil, "", 0, 0, errf("server", "channel", "产物图格式无法识别（可能被 WAF 拦截）")
	}
	return raw, mime, w, h, nil
}

// sniffImage 按魔数判 mime；jpeg/png 用 image.DecodeConfig 拿尺寸（webp 尺寸不解析，返回 0）。
func sniffImage(raw []byte) (mime string, w, h int) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8}):
		mime = "image/jpeg"
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}):
		mime = "image/png"
	case len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return "image/webp", 0, 0
	case bytes.HasPrefix(raw, []byte("GIF8")):
		return "image/gif", 0, 0
	default:
		return "", 0, 0
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		w, h = cfg.Width, cfg.Height
	}
	return mime, w, h
}

func ratioOr(r string) string {
	if r == "" {
		return "1:1"
	}
	return r
}

func extractString(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return ""
}

func truncJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
