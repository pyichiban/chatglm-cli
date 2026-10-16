// Package imgutil 提供「多参考图拼版 + 图床上传」能力，供一键生图命令使用。
//
// 拼版算法（contract.json image_pipeline）：
//   - justified 行布局：按输入图宽高比贪心分行，行内等高缩放、行宽对齐画布，
//     二分搜索最大画宽使得 总面积 ≤ 预算 且 单图放大倍数 ≤ UpscaleCap ⇒ 内容像素最大化；
//   - 每张参考图：方形 outline 画在图像**外侧**（不遮有效像素）+ 下方标注条 reference1..N；
//   - 最终 JPEG 面积不超过预算（默认 480w，留余量低于上游 500w 硬约束）。
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
	"sort"

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
	// BudgetTarget 实际瞄准的填充比例（留 2% 余量给编码器/取整）。
	BudgetTarget = 0.98
	// StripH 是每行下方标注条高度（含 3x 文字）。
	StripH = 48
	// Gap 是同行两张图之间的间距；Margin 是画布外边距。
	Gap    = 24
	Margin = 16
	// OutlineGap/OutlineW 方形外框：外扩距离与线宽（严格在图像外侧，不遮像素）。
	OutlineGap = 6
	OutlineW   = 4
	// UpscaleCap 单图最大放大倍数（避免小图被拉大成纯模糊）。
	UpscaleCap = 4.0
)

// Placement 是一张参考图在拼版画布上的最终位置（缩放后尺寸）。
type Placement struct {
	Index int // 原始顺序（0-based），标注为 reference{Index+1}
	X, Y  int
	W, H  int
}

type layout struct {
	W, H       int
	Placements []Placement
	UsedPixels int // Σ 缩放后图像面积（衡量填充率）
}

// BuildRefSheet 把多张参考图拼成一张 JPEG：等比例缩放 + 方形外框 + reference1..N 标注。
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
	lay := BestLayout(imgs, PixelBudget)

	sheet := image.NewRGBA(image.Rect(0, 0, lay.W, lay.H))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	red := color.RGBA{R: 220, A: 255}
	for _, p := range lay.Placements {
		im := imgs[p.Index]
		scaled := scaleTo(im, p.W, p.H)
		r := image.Rect(p.X, p.Y, p.X+p.W, p.Y+p.H)
		draw.Draw(sheet, r, scaled, scaled.Bounds().Min, draw.Over)
		// 方形外框：画在图像外侧（外扩 OutlineGap），绝不覆盖有效像素
		drawRectOutline(sheet,
			image.Rect(p.X-OutlineGap, p.Y-OutlineGap, p.X+p.W+OutlineGap, p.Y+p.H+OutlineGap),
			OutlineW, red)
		// 标注条：图像正下方（strip 高度已计入行高），深灰底 + 白字 referenceN
		barTop := p.Y + p.H + OutlineGap + OutlineW
		bar := image.Rect(p.X-OutlineGap, barTop, p.X+p.W+OutlineGap, p.Y+p.H+StripH)
		draw.Draw(sheet, bar, image.NewUniform(color.RGBA{R: 40, G: 40, B: 40, A: 255}),
			image.Point{}, draw.Src)
		drawLabel(sheet, fmt.Sprintf("reference%d", p.Index+1), bar.Dx()/2+bar.Min.X, (bar.Min.Y+bar.Max.Y)/2)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 92}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), lay.W, lay.H, nil
}

// BestLayout 在候选画布纵横比里选「内容像素最大化」的 justified 行布局。
// 纯函数（只依赖输入图像的尺寸），可测。
func BestLayout(imgs []image.Image, budget int) layout {
	ratios := canvasAspectCandidates(imgs)
	var best layout
	for _, R := range ratios {
		if l, ok := searchWidth(imgs, R, budget); ok && l.UsedPixels > best.UsedPixels {
			best = l
		}
	}
	if best.Placements == nil { // 兜底（理论不可达）：最小布局
		best, _ = searchWidth(imgs, 1.0, 1<<20)
	}
	return best
}

// searchWidth 在几何递增的画宽阶梯上扫描，返回「内容像素最大化」的布局。
// used(W) 非单调（行高被放大上限鍳住后，增大 W 只会产生留白），故用阶梯扫描而非二分。
func searchWidth(imgs []image.Image, R float64, budget int) (layout, bool) {
	var best layout
	ok := false
	for W := 64.0; W <= 8192.0; W *= 1.25 {
		if l, feasible := tryLayout(imgs, W, R, budget); feasible && l.UsedPixels > best.UsedPixels {
			best, ok = l, true
		}
	}
	return best, ok
}

// tryLayout 以行高驱动值 W、画布纵横比 R 做 justified 行布局。
// 行成员只由 R 决定；行高 = min(等高值, 该行放大上限)，画布宽由最宽的行决定（内容驱动，
// 被鍳住的行允许右侧留白，把预算让给大图）。
func tryLayout(imgs []image.Image, W float64, R float64, budget int) (layout, bool) {
	if len(imgs) == 0 {
		return layout{}, false
	}
	usable := W - 2*Margin
	if usable < 64 {
		return layout{}, false
	}
	H := 2 * Margin
	canvasW := 0
	placements := make([]Placement, 0, len(imgs))
	used := 0
	flushRow := func(row []int) bool {
		if len(row) == 0 {
			return true
		}
		sum := 0.0
		for _, i := range row {
			sum += aspect(imgs[i])
		}
		hrCap := math.Inf(1)
		for _, i := range row { // 行高鍳制：任一图放大不得超过 UpscaleCap
			hrCap = math.Min(hrCap, UpscaleCap*float64(origH(imgs[i])))
		}
		hr := math.Min(usable/sum, hrCap)
		x := Margin
		rowW := 0
		for _, i := range row {
			w := int(float64(hr) * aspect(imgs[i]))
			if w < 1 {
				w = 1
			}
			placements = append(placements, Placement{Index: i, X: x, Y: int(H), W: w, H: int(hr)})
			used += w * int(hr)
			x += w + Gap
			rowW = x - Gap
		}
		if rowW > canvasW {
			canvasW = rowW
		}
		H += int(hr) + StripH
		return true
	}
	row, rowSum := []int{}, 0.0
	for i, im := range imgs {
		_ = im
		row = append(row, i)
		rowSum += aspect(im)
		if rowSum >= R { // 行累计纵横比达到画布纵横比 ⇒ 结行（填充率最高）
			flushRow(row)
			row, rowSum = nil, 0
		}
	}
	flushRow(row)
	l := layout{W: canvasW + 2*Margin, H: int(H), Placements: placements, UsedPixels: used}
	if float64(l.W)*float64(l.H) > float64(budget)*BudgetTarget {
		return layout{}, false
	}
	return l, true
}

// canvasAspectCandidates 候选画布纵横比：常见比例 + 输入图纵横比的几何平均。
func canvasAspectCandidates(imgs []image.Image) []float64 {
	geo := 1.0
	for _, im := range imgs {
		geo *= aspect(im)
	}
	geo = math.Pow(geo, 1/float64(len(imgs)))
	set := map[int64]bool{}
	var out []float64
	for _, r := range []float64{1.0, 4.0 / 3, 3.0 / 4, 16.0 / 9, 9.0 / 16, 3.0 / 2, 2.0 / 3, geo} {
		k := int64(math.Round(r * 1000))
		if !set[k] {
			set[k] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func aspect(im image.Image) float64 {
	b := im.Bounds()
	if b.Dy() == 0 {
		return 1
	}
	return float64(b.Dx()) / float64(b.Dy())
}

func origH(im image.Image) int { return im.Bounds().Dy() }

func decodeImage(path string) (image.Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	im, _, err := image.Decode(bytes.NewReader(b))
	return im, err
}

// scaleTo 精确缩放到 w×h（布局阶段已保证等比例；CatmullRom 画质优先）。
func scaleTo(im image.Image, w, h int) image.Image {
	b := im.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return im
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), im, b, xdraw.Over, nil)
	return dst
}

// drawRectOutline 方形描边：只画四条边所在的环带（严格在 rect 外部调用时不会遮像素）。
func drawRectOutline(dst *image.RGBA, r image.Rectangle, thickness int, c color.RGBA) {
	for i := 0; i < thickness; i++ {
		// 外扩第 i 圈
		top := image.Rect(r.Min.X-i, r.Min.Y-i, r.Max.X+i, r.Min.Y-i+1)
		bot := image.Rect(r.Min.X-i, r.Max.Y+i-1, r.Max.X+i, r.Max.Y+i)
		left := image.Rect(r.Min.X-i, r.Min.Y-i, r.Min.X-i+1, r.Max.Y+i)
		right := image.Rect(r.Max.X+i-1, r.Min.Y-i, r.Max.X+i, r.Max.Y+i)
		for _, rr := range []image.Rectangle{top, bot, left, right} {
			draw.Draw(dst, rr, image.NewUniform(c), image.Point{}, draw.Over)
		}
	}
}

// drawLabel 用 basicfont 渲染文字（3x 放大）后居中贴到 (cx,cy)。
func drawLabel(dst *image.RGBA, text string, cx, cy int) {
	f := basicfont.Face7x13
	d := &font.Drawer{Src: image.NewUniform(color.White), Face: f}
	w := d.MeasureString(text).Ceil()
	tmp := image.NewRGBA(image.Rect(0, 0, w+2, 15))
	d.Dst = tmp
	d.Dot = fixed.P(1, 13)
	d.DrawString(text)

	const k = 3
	scaled := image.NewRGBA(image.Rect(0, 0, tmp.Bounds().Dx()*k, tmp.Bounds().Dy()*k))
	xdraw.NearestNeighbor.Scale(scaled, scaled.Bounds(), tmp, tmp.Bounds(), xdraw.Over, nil)

	dx, dy := cx-scaled.Bounds().Dx()/2, cy-scaled.Bounds().Dy()/2
	draw.Draw(dst, image.Rect(dx, dy, dx+scaled.Bounds().Dx(), dy+scaled.Bounds().Dy()),
		scaled, scaled.Bounds().Min, draw.Over)
}
