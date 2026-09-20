package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 登录验证码：一张「a + b = ?」的 PNG，答案放在 HMAC 签名的 cookie 里。
//
// 为什么要自己画：不引第三方包、不依赖字体文件（字体文件是要随包分发的资产，
// 而且删了主题字体之后更不该为验证码再引一个）。算式只用 10 个数字和两个符号，
// 用点阵字形画出来即可。
//
// 为什么是 PNG 而不是 SVG：SVG 是文本，验证码的内容会被脚本直接读走——
// 那等于没有验证码。

const (
	// CaptchaCookie 是存放验证码答案的 cookie 名。
	CaptchaCookie = "gmt_captcha"
	// CaptchaTTL 是验证码有效期。
	CaptchaTTL = 5 * time.Minute
)

// SetCaptchaCookie 下发验证码答案的 cookie（HttpOnly：只有服务端读得到）。
func SetCaptchaCookie(c *gin.Context, value string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     CaptchaCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(CaptchaTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCaptcha 让当前验证码立即作废：验证过（成功或失败）都要清掉，
// 否则一个答案在整个有效期内可以被反复使用。
func ClearCaptcha(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: CaptchaCookie, Value: "", Path: "/", MaxAge: -1})
}

// CaptchaImage 生成一张新的验证码图片，返回 PNG 数据与要下发的 cookie 值。
func CaptchaImage(secret string) (imageBytes []byte, cookie string, err error) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	a := rnd.Intn(9) + 1
	b := rnd.Intn(9) + 1
	text := fmt.Sprintf("%d + %d = ?", a, b)

	img := renderText(text, rnd)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), captchaCookieValue(secret, a+b), nil
}

// VerifyCaptcha 校验用户填写的答案（同时校验签名与有效期）。
//
// cookie 里放的是「答案的 HMAC」，不是答案本身：签名 cookie 客户端是能读的，
// 一旦把答案写进去，脚本拿自己的 cookie 就能直接读出答案——等于没有验证码。
// 答案域很小（两个 1..9 相加，2..18），校验时把候选值逐个算一遍签名即可，
// 既能验对错，又不泄露答案。
func VerifyCaptcha(secret, cookie, answer string) bool {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	// 用户可能填 "8" 或 "08" 或带空格：能算对就行，不做格式上的刁难。
	got, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil {
		return false
	}
	for candidate := -99; candidate <= 999; candidate++ {
		if candidate != got {
			continue
		}
		if hmac.Equal([]byte(captchaSig(secret, parts[1], exp, candidate)), []byte(parts[2])) {
			return true
		}
	}
	if debugOn() {
		// 只打「校验失败」的形状，不打 secret 本身。
		log.Printf("captcha: 校验失败 exp=%d now=%d nonce=%q sig=%q answer=%q secretLen=%d",
			exp, time.Now().Unix(), parts[1], parts[2], answer, len(secret))
	}
	return false
}

// debugOn 打开验证码调试日志（仅本地排查用：GMT_DEBUG_CAPTCHA=1）。
func debugOn() bool { return os.Getenv("GMT_DEBUG_CAPTCHA") != "" }

// captchaCookieValue 生成 cookie 值：过期时间.随机串.答案签名。
func captchaCookieValue(secret string, answer int) string {
	exp := time.Now().Add(CaptchaTTL).Unix()
	nonce := strconv.FormatInt(rand.Int63(), 36)
	return fmt.Sprintf("%d.%s.%s", exp, nonce, captchaSig(secret, nonce, exp, answer))
}

func captchaSig(secret, nonce string, exp int64, answer int) string {
	raw := fmt.Sprintf("captcha:%s.%d.%d", nonce, exp, answer)
	sum := hmac.New(sha256.New, []byte(secret))
	sum.Write([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum.Sum(nil))
}

// ---- 点阵字形 ----

// 5x7 点阵：'#' 为墨点。验证码只需要数字和 + = ? 这几个字形。
var glyphs = map[rune][]string{
	'0': {".###.", "#...#", "#..##", "#.#.#", "##..#", "#...#", ".###."},
	'1': {"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'2': {".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
	'3': {"#####", "...#.", "..#..", "...#.", "....#", "#...#", ".###."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'5': {"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
	'6': {"..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'8': {".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."},
	'+': {".....", "..#..", "..#..", "#####", "..#..", "..#..", "....."},
	'=': {".....", ".....", "#####", ".....", "#####", "....."},
	'?': {".###.", "#...#", "....#", "...#.", "..#..", ".....", "..#.."},
}

const (
	glyphW  = 5
	glyphH  = 7
	scale   = 3
	gap     = 5
	padding = 10
)

func renderText(text string, rnd *rand.Rand) *image.RGBA {
	width := padding*2 + len([]rune(text))*glyphW*scale + (len([]rune(text))-1)*gap
	height := padding*2 + glyphH*scale
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// 底色：随机浅色，避免固定底色被当成模板匹配。
	bg := color.RGBA{R: uint8(238 + rnd.Intn(16)), G: uint8(240 + rnd.Intn(14)), B: uint8(242 + rnd.Intn(12)), A: 255}
	fill(img, bg)

	x := padding
	for _, ch := range text {
		g, ok := glyphs[ch]
		if !ok {
			x += glyphW*scale + gap
			continue
		}
		ink := color.RGBA{R: uint8(30 + rnd.Intn(60)), G: uint8(40 + rnd.Intn(60)), B: uint8(60 + rnd.Intn(80)), A: 255}
		y0 := padding + rnd.Intn(4) - 2
		for row, line := range g {
			// 每行按行号做一点横向错位，看起来像手写而不是印刷体。
			shift := rnd.Intn(3) - 1
			for col, c := range line {
				if c != '#' {
					continue
				}
				px := x + col*scale + shift
				py := y0 + row*scale
				block(img, px, py, scale, ink)
			}
		}
		x += glyphW*scale + gap
	}

	// 干扰线 + 噪点：把「按字形切图识别」挡住。
	for i := 0; i < 5; i++ {
		c := color.RGBA{R: uint8(120 + rnd.Intn(120)), G: uint8(120 + rnd.Intn(120)), B: uint8(120 + rnd.Intn(120)), A: 255}
		line(img, rnd.Intn(width), rnd.Intn(height), rnd.Intn(width), rnd.Intn(height), c)
	}
	for i := 0; i < 120; i++ {
		c := color.RGBA{R: uint8(rnd.Intn(200)), G: uint8(rnd.Intn(200)), B: uint8(rnd.Intn(200)), A: 255}
		img.Set(rnd.Intn(width), rnd.Intn(height), c)
	}
	return img
}

func fill(img *image.RGBA, c color.Color) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

func block(img *image.RGBA, x0, y0, size int, c color.Color) {
	b := img.Bounds()
	for y := y0; y < y0+size; y++ {
		for x := x0; x < x0+size; x++ {
			if x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y {
				img.Set(x, y, c)
			}
		}
	}
}

// line 画一条 Bresenham 直线（干扰线用）。
func line(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
