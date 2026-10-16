// Package imgutil 提供「多参考图拼版 + 图床上传」能力，供一键生图命令使用。
//
// 拼版规则（contract.json image_pipeline）：
//   - 所有参考图等比例缩放后拼进网格（每格下方标注 reference1..N，图外画红圈）；
//   - 最终 JPEG 面积不超过预算像素（默认 480w，留余量低于上游 500w 硬约束）；
//   - 单张参考图也拼版：统一获得红圈/标注与超限降采样。
package imgutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册 gif 解码
	"image/jpeg"
	_ "image/png" // 注册 png 解码
	"math"
	"os"

	_ "golang.org/x/image/bmp"      // 注册 bmp 解码
	xdraw "golang.org/x/image/draw" // 高质量缩放（CatmullRom）
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/tiff" // 注册 tiff 解码
	_ "golang.org/x/image/webp" // 注册 webp 解码
)

const (
	// PixelBudget 是拼版结果的像素面积上限（上游 >500w 会静默异常，留余量）。
	PixelBudget = 4_800_000
	labelH      = 44 // 每格标注条高度（含 3x 文字）
	pad         = 12 // 外边距/格间距
)

// PlanGrid 计算网格与单格尺寸，保证 总面积(含标注条与边距) ≤ budget。纯函数，可测。
func PlanGrid(n, budget int) (cols, rows, cellW, cellH int) {
	cols = int(math.Ceil(math.Sqrt(float64(n))))
	rows = (n + cols - 1) / cols
	// 解 (cols*x+pad*(cols+1)) * (rows*(x+labelH)+pad*(rows+1)) ≤ budget 的近似解
	x := int(math.Sqrt(float64(budget)/float64(rows*cols))) - labelH
	for x > 16 {
		w := cols*x + pad*(cols+1)
		h := rows*(x+labelH) + pad*(rows+1)
		if w*h <= budget {
			break
		}
		x--
	}
	return cols, rows, x, x
}

// BuildRefSheet 把多张参考图拼成一张 JPEG：等比例缩放 + 红圈 + reference1..N 标注。
// 返回 JPEG 字节与最终尺寸。
func BuildRefSheet(paths []string) ([]byte, int, int, error) {
	if len(paths) == 0 {
		return nil, 0, 0, fmt.Errorf("没有参考图")
	}
	imgs := make([]image.Image, 0, len(paths))
	for i, p := range paths {
		im, err := decodeImage(p)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("参考图 %d (%s): %w", i+1, p, err)
		}
		imgs = append(imgs, im)
	}
	cols, rows, cw, ch := PlanGrid(len(imgs), PixelBudget)

	W := cols*cw + pad*(cols+1)
	H := rows*(ch+labelH) + pad*(rows+1)
	sheet := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	for i, im := range imgs {
		row, col := i/cols, i%cols
		x0 := pad + col*(cw+pad)
		y0 := pad + row*(ch+labelH+pad)
		// 等比例缩放到格内
		dst := scaleFit(im, cw, ch)
		b := dst.Bounds()
		dx := x0 + (cw-b.Dx())/2
		dy := y0 + (ch-b.Dy())/2
		draw.Draw(sheet, image.Rect(dx, dy, dx+b.Dx(), dy+b.Dy()), dst, b.Min, draw.Over)
		// 红圈：围绕贴入的图像（外扩 8px）
		drawEllipse(sheet, dx+b.Dx()/2, dy+b.Dy()/2,
			b.Dx()/2+8, b.Dy()/2+8, 3, color.RGBA{R: 230, A: 255})
		// 标注条：深灰底 + 白字 referenceN（3x 放大）
		lx, ly := x0, y0+ch+4
		draw.Draw(sheet, image.Rect(lx, ly, lx+cw, ly+labelH-8),
			image.NewUniform(color.RGBA{R: 40, G: 40, B: 40, A: 255}), image.Point{}, draw.Src)
		label := fmt.Sprintf("reference%d", i+1)
		drawLabel(sheet, label, lx+cw/2, ly+(labelH-8)/2)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 92}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), W, H, nil
}

func decodeImage(path string) (image.Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	im, _, err := image.Decode(bytes.NewReader(b))
	return im, err
}

// scaleFit 等比例缩放到 ≤w×h（CatmullRom，画质优先）。
func scaleFit(im image.Image, w, h int) image.Image {
	b := im.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= w && sh <= h {
		return im // 不放大：保持原始清晰度
	}
	k := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
	dw, dh := max16(1, int(float64(sw)*k)), max16(1, int(float64(sh)*k))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), im, b, xdraw.Over, nil)
	return dst
}

func max16(a, b int) int { // 防止缩放结果为 0
	if a > b {
		return a
	}
	return b
}

// drawEllipse 中点法椭圆描边（thickness 像素粗）。
func drawEllipse(dst *image.RGBA, cx, cy, rx, ry, thickness int, c color.RGBA) {
	if rx <= 0 || ry <= 0 {
		return
	}
	step := 0.002
	for t := 0.0; t < 2*math.Pi; t += step {
		x := cx + int(float64(rx)*math.Cos(t))
		y := cy + int(float64(ry)*math.Sin(t))
		for dx := 0; dx < thickness; dx++ {
			for dy := 0; dy < thickness; dy++ {
				dst.Set(x+dx, y+dy, c)
			}
		}
	}
}

// drawLabel 用 basicfont 渲染文字（3x 放大）后居中贴到 (cx,cy)。
func drawLabel(dst *image.RGBA, text string, cx, cy int) {
	f := basicfont.Face7x13
	d := &font.Drawer{Src: image.NewUniform(color.White), Face: f}
	adv := d.MeasureString(text)
	w := adv.Ceil()
	tmp := image.NewRGBA(image.Rect(0, 0, w+2, 13+2))
	d.Dst = tmp
	d.Dot = fixed.P(1, 12)
	d.DrawString(text)

	const k = 3 // 放大倍数
	scaled := image.NewRGBA(image.Rect(0, 0, tmp.Bounds().Dx()*k, tmp.Bounds().Dy()*k))
	xdraw.NearestNeighbor.Scale(scaled, scaled.Bounds(), tmp, tmp.Bounds(), xdraw.Over, nil)

	dx, dy := cx-scaled.Bounds().Dx()/2, cy-scaled.Bounds().Dy()/2
	draw.Draw(dst, image.Rect(dx, dy, dx+scaled.Bounds().Dx(), dy+scaled.Bounds().Dy()),
		scaled, scaled.Bounds().Min, draw.Over)
}
