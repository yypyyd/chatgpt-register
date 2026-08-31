package luminareg

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

// 滑块验证码的图像匹配参数。
const (
	// pieceSkipLeft 滑块起始位置右侧这么多像素内不参与匹配：缺口不可能落在滑块自己身上。
	pieceSkipLeft = 25
	// rowBand 缺口纵坐标由 DOM 推算，允许上下浮动这么多像素再匹配。
	rowBand = 8
	// ringWidth 缺口外圈参考带宽度（像素）：BytePlus 的缺口是压暗的图块 + 亮描边，
	// 用「外圈亮度 - 内部亮度」定位比纹理匹配稳得多。
	ringWidth = 4
)

// grayImage 灰度图 + alpha 通道（滑块图只有不透明区域才是拼图块）。
type grayImage struct {
	w, h  int
	pix   []float64
	alpha []uint8
}

func (g *grayImage) at(x, y int) float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= g.w {
		x = g.w - 1
	}
	if y >= g.h {
		y = g.h - 1
	}
	return g.pix[y*g.w+x]
}

func toGray(img image.Image) *grayImage {
	b := img.Bounds()
	g := &grayImage{w: b.Dx(), h: b.Dy()}
	g.pix = make([]float64, g.w*g.h)
	g.alpha = make([]uint8, g.w*g.h)
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			r, gg, bb, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			idx := y*g.w + x
			g.pix[idx] = 0.299*float64(r>>8) + 0.587*float64(gg>>8) + 0.114*float64(bb>>8)
			g.alpha[idx] = uint8(a >> 8)
		}
	}
	return g
}

// fetchImage 下载验证码图片；带上与浏览器一致的 UA/Referer，并跟随任务代理出口。
func fetchImage(rawURL, proxy string) (image.Image, error) {
	tr := &http.Transport{}
	if proxy != "" {
		pu, err := url.Parse(normalizeProxy(proxy))
		if err != nil {
			return nil, fmt.Errorf("解析代理失败: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", luminaURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载验证码图片失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码验证码图片失败: %w", err)
	}
	return img, nil
}

// solveOffset 在背景图里定位缺口，返回滑块需要横移的原图像素数与匹配得分。
// 缺口是按拼图块形状压暗并带亮描边的区域，所以按形状取「外圈平均亮度 - 内部平均亮度」
// 打分：真缺口处内部明显更暗、描边更亮，得分远高于普通图像内容。
// holeY 是滑块图画布顶边相对背景图顶边的纵向偏移（原始尺寸像素），由 DOM 位置换算而来；
// 传负值表示纵坐标未知（协议流程拿到的是独立小图），此时全图扫行。
func solveOffset(bg, piece image.Image, holeY float64) (int, float64, error) {
	bgG := toGray(bg)
	pcG := toGray(piece)

	x0, y0, x1, y1, ok := alphaBounds(pcG)
	if !ok {
		return 0, 0, fmt.Errorf("滑块图没有可用的拼图块区域")
	}
	tw, th := x1-x0+1, y1-y0+1
	if tw < 8 || th < 8 || tw > bgG.w || th > bgG.h {
		return 0, 0, fmt.Errorf("拼图块尺寸异常: %dx%d", tw, th)
	}

	inside, ring, insideN, ringN := shapeMasks(pcG, x0, y0, tw, th)
	if insideN < 64 || ringN < 32 {
		return 0, 0, fmt.Errorf("拼图块有效像素太少: 内部 %d 外圈 %d", insideN, ringN)
	}

	yTop := int(math.Round(holeY)) + y0
	yFrom, yTo := yTop-rowBand, yTop+rowBand
	if yFrom < 0 {
		yFrom = 0
	}
	if yTo > bgG.h-th {
		yTo = bgG.h - th
	}
	if yFrom > yTo || holeY < 0 {
		yFrom, yTo = 0, bgG.h-th
	}
	xFrom := x0 + pieceSkipLeft
	xTo := bgG.w - tw
	if xFrom > xTo {
		return 0, 0, fmt.Errorf("背景图宽度不足，无法匹配")
	}

	bestScore, bestX := -math.MaxFloat64, xFrom
	pw, ph := tw+2*ringWidth, th+2*ringWidth
	for y := yFrom; y <= yTo; y++ {
		for x := xFrom; x <= xTo; x++ {
			var inSum, ringSum float64
			for py := 0; py < ph; py++ {
				base := py * pw
				gy := y - ringWidth + py
				for px := 0; px < pw; px++ {
					switch {
					case inside[base+px]:
						inSum += bgG.at(x-ringWidth+px, gy)
					case ring[base+px]:
						ringSum += bgG.at(x-ringWidth+px, gy)
					}
				}
			}
			score := ringSum/float64(ringN) - inSum/float64(insideN)
			if score > bestScore {
				bestScore, bestX = score, x
			}
		}
	}
	return bestX - x0, bestScore, nil
}

// shapeMasks 返回拼图块形状（含四周 ringWidth 边距的画布）的内部掩码与外圈掩码。
func shapeMasks(pcG *grayImage, x0, y0, tw, th int) (inside, ring []bool, insideN, ringN int) {
	pw, ph := tw+2*ringWidth, th+2*ringWidth
	inside = make([]bool, pw*ph)
	ring = make([]bool, pw*ph)
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			if pcG.alpha[(y0+y)*pcG.w+(x0+x)] > 10 {
				inside[(y+ringWidth)*pw+(x+ringWidth)] = true
				insideN++
			}
		}
	}
	// 外圈 = 形状膨胀 ringWidth 后减去形状本身。
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			if inside[y*pw+x] {
				continue
			}
			near := false
			for dy := -ringWidth; dy <= ringWidth && !near; dy++ {
				for dx := -ringWidth; dx <= ringWidth; dx++ {
					ny, nx := y+dy, x+dx
					if ny < 0 || nx < 0 || ny >= ph || nx >= pw {
						continue
					}
					if inside[ny*pw+nx] {
						near = true
						break
					}
				}
			}
			if near {
				ring[y*pw+x] = true
				ringN++
			}
		}
	}
	return inside, ring, insideN, ringN
}

// alphaBounds 返回滑块图里不透明像素的包围盒。
func alphaBounds(g *grayImage) (x0, y0, x1, y1 int, ok bool) {
	x0, y0 = g.w, g.h
	x1, y1 = -1, -1
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			if g.alpha[y*g.w+x] <= 10 {
				continue
			}
			if x < x0 {
				x0 = x
			}
			if x > x1 {
				x1 = x
			}
			if y < y0 {
				y0 = y
			}
			if y > y1 {
				y1 = y
			}
		}
	}
	return x0, y0, x1, y1, x1 >= x0 && y1 >= y0
}
