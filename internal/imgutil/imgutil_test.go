package imgutil

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 生成一张测试图（纯色 + 尺寸）。
func mkImg(t *testing.T, dir, name string, w, h int, c color.RGBA) string {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	p := filepath.Join(dir, name)
	f, _ := os.Create(p)
	if err := jpeg.Encode(f, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func mustImg(t *testing.T, p string) image.Image {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	im, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// TestLayoutFillsBudget：常规尺寸多图，拼版必须接近用满预算（填充率 ≥85%），
// 且所有 placement 都在画布内、互不重叠。
func TestLayoutFillsBudget(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		mkImg(t, dir, "a.jpg", 1600, 1200, color.RGBA{R: 200, A: 255}), // 4:3
		mkImg(t, dir, "b.jpg", 1200, 1600, color.RGBA{B: 200, A: 255}), // 3:4
		mkImg(t, dir, "c.jpg", 1500, 1000, color.RGBA{G: 200, A: 255}), // 3:2
	}
	imgs := []image.Image{mustImg(t, paths[0]), mustImg(t, paths[1]), mustImg(t, paths[2])}
	lay := BestLayout(imgs, PixelBudget)

	if fill := float64(lay.UsedPixels) / float64(lay.W*lay.H); fill < 0.85 {
		t.Errorf("填充率 %.2f 过低（布局浪费空间）", fill)
	}
	if px := lay.W * lay.H; px < int(0.80*PixelBudget) {
		t.Errorf("画布 %d px 远小于预算 %d（没有尽量撑满）", px, PixelBudget)
	}
	// 互不重叠 + 画布内
	seen := map[[4]int]bool{}
	for _, p := range lay.Placements {
		if p.X < 0 || p.Y < 0 || p.X+p.W > lay.W || p.Y+p.H > lay.H {
			t.Errorf("placement 越界: %+v (canvas %dx%d)", p, lay.W, lay.H)
		}
		for o := range seen {
			if !(p.X+p.W <= o[0] || o[2] <= p.X || p.Y+p.H <= o[1] || o[3] <= p.Y) {
				t.Errorf("placement 重叠: %+v vs %v", p, o)
			}
		}
		seen[[4]int{p.X, p.Y, p.W, p.H}] = true
	}
}

// TestLayoutUpscaleCap：小图不被拉大超过 UpscaleCap 倍。
func TestLayoutUpscaleCap(t *testing.T) {
	dir := t.TempDir()
	imgs := []image.Image{mustImg(t, mkImg(t, dir, "tiny.jpg", 160, 255, color.RGBA{A: 255}))}
	lay := BestLayout(imgs, PixelBudget)
	for _, p := range lay.Placements {
		if float64(p.W)/160 > UpscaleCap+0.01 || float64(p.H)/255 > UpscaleCap+0.01 {
			t.Errorf("放大超上限: %+v", p)
		}
	}
}

// TestBuildRefSheet：合法 JPEG、红框只存在于图像外侧（不遮有效像素）、标注条存在。
func TestBuildRefSheet(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		mkImg(t, dir, "a.jpg", 800, 600, color.RGBA{R: 60, G: 60, B: 200, A: 255}), // 蓝图1
		mkImg(t, dir, "b.jpg", 600, 800, color.RGBA{R: 60, G: 200, B: 60, A: 255}), // 绿图2
	}
	b, w, h, err := BuildRefSheet(paths)
	if err != nil {
		t.Fatal(err)
	}
	if w*h > PixelBudget {
		t.Errorf("拼版 %dx%d 超预算", w, h)
	}
	im, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("产物不是合法 JPEG: %v", err)
	}
	if im.Bounds().Dx() != w || im.Bounds().Dy() != h {
		t.Errorf("声明尺寸 %dx%d 与实际 %v 不符", w, h, im.Bounds())
	}

	lay := BestLayout([]image.Image{mustImg(t, paths[0]), mustImg(t, paths[1])}, PixelBudget)
	redInside := false
	darkBar := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := im.At(x, y).RGBA()
			isRed := r > 50000 && g < 30000 && bl < 30000
			// 红色像素不得落在任何 placement 内部（outline 不遮有效像素）
			if isRed {
				for _, p := range lay.Placements {
					if x >= p.X && x < p.X+p.W && y >= p.Y && y < p.Y+p.H {
						redInside = true
					}
				}
			}
			// 标注条：深灰底
			if r < 25000 && g < 25000 && bl < 25000 {
				darkBar++
			}
		}
	}
	if redInside {
		t.Errorf("红色 outline 覆盖了参考图有效像素")
	}
	if darkBar < 500 {
		t.Errorf("找不到标注条底色（referenceN 未绘制？）")
	}
}

func TestBuildRefSheetEmpty(t *testing.T) {
	if _, _, _, err := BuildRefSheet(nil); err == nil {
		t.Errorf("空参考图列表应报错")
	}
	if _, _, _, err := BuildRefSheet([]string{filepath.Join(t.TempDir(), "nope.jpg")}); err == nil {
		t.Errorf("不存在的文件应报错")
	}
}

// TestUploadJPEGFallback：meitu 失败（code!=200）⇒ 自动换 icve；请求体必须是 raw binary。
func TestUploadJPEGFallback(t *testing.T) {
	var bodies [][]byte
	var types []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		bodies = append(bodies, b)
		types = append(types, r.Header.Get("X-Upload-Type"))
		if r.Header.Get("X-File-Name") == "" || r.Header.Get("X-File-Size") == "" {
			t.Errorf("缺少 X-File-Name/X-File-Size 头")
		}
		if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("Content-Type = %q", ct)
		}
		if len(types) == 1 {
			_, _ = w.Write([]byte(`{"code":500,"msg":"meitu 上传失败"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","data":"https://file.icve.com.cn/x.jpg"}`))
	}))
	defer srv.Close()
	old := sangUploadURL
	sangUploadURL = srv.URL
	defer func() { sangUploadURL = old }()

	payload := []byte("fake-jpeg-bytes-0123456789")
	url, ch, err := UploadJPEG(payload)
	if err != nil {
		t.Fatalf("回退后应成功: %v", err)
	}
	if ch != "icve" || url != "https://file.icve.com.cn/x.jpg" {
		t.Errorf("ch=%s url=%s", ch, url)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], payload) || !bytes.Equal(bodies[1], payload) {
		t.Errorf("请求体必须是原始字节且逐字节相同: %v", bodies)
	}
	if types[0] != "meitu" || types[1] != "icve" {
		t.Errorf("通道顺序错: %v", types)
	}
}

// TestUploadJPEGAllFail：全部通道失败 ⇒ 汇总错误。
func TestUploadJPEGAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"down"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := sangUploadURL
	sangUploadURL = srv.URL
	defer func() { sangUploadURL = old }()

	if _, _, err := UploadJPEG([]byte("x")); err == nil {
		t.Errorf("全通道失败必须报错")
	}
}

func TestParseSangResp(t *testing.T) {
	code, data, err := parseSangResp([]byte(`{"code":200,"msg":"success","data":"https://x/y.jpg"}`))
	if err != nil || code != 200 || data != "https://x/y.jpg" {
		t.Errorf("code=%d data=%q err=%v", code, data, err)
	}
}
