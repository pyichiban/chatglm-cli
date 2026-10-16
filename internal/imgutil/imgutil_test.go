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

// TestPlanGridBudget：任意 1..9 张图，拼版面积都必须 ≤ 预算，且格子不会小到不可用。
func TestPlanGridBudget(t *testing.T) {
	for n := 1; n <= 9; n++ {
		cols, rows, cw, ch := PlanGrid(n, PixelBudget)
		if cols*rows < n {
			t.Errorf("n=%d: 网格 %dx%d 放不下", n, cols, rows)
		}
		w := cols*cw + pad*(cols+1)
		h := rows*(ch+labelH) + pad*(rows+1)
		if w*h > PixelBudget {
			t.Errorf("n=%d: 拼版 %dx%d=%d 超预算 %d", n, w, h, w*h, PixelBudget)
		}
		if cw < 32 || ch < 32 {
			t.Errorf("n=%d: 格子过小 %dx%d", n, cw, ch)
		}
	}
}

// TestBuildRefSheet：两张参考图 → JPEG 可解码、尺寸在预算内、字节数合理。
func TestBuildRefSheet(t *testing.T) {
	dir := t.TempDir()
	p1 := mkImg(t, dir, "a.jpg", 800, 600, color.RGBA{R: 200, A: 255})
	p2 := mkImg(t, dir, "b.png", 300, 900, color.RGBA{B: 200, A: 255})

	b, w, h, err := BuildRefSheet([]string{p1, p2})
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
	if len(b) < 10_000 {
		t.Errorf("产物过小，疑似空白图: %d bytes", len(b))
	}
	// 红圈像素存在性：在贴图区周围应能找到接近纯红的像素
	found := false
	for y := 0; y < h && !found; y++ {
		for x := 0; x < w && !found; x++ {
			r, g, bl, _ := im.At(x, y).RGBA()
			if r > 60000 && g < 20000 && bl < 20000 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("找不到红圈像素（标注/圈未绘制？）")
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

// 保留一个 JSON 解析冒烟（与 glm 侧信封无耦合）。
func TestParseSangResp(t *testing.T) {
	code, data, err := parseSangResp([]byte(`{"code":200,"msg":"success","data":"https://x/y.jpg"}`))
	if err != nil || code != 200 || data != "https://x/y.jpg" {
		t.Errorf("code=%d data=%q err=%v", code, data, err)
	}
}
