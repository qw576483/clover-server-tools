package auth

import (
	"bytes"
	"image/png"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 验证码机制的回归测试：重点不是「画得好不好看」，而是
//  1. 生成的图片是合法 PNG；
//  2. cookie 里**不含答案本身**（只答对才能通过）；
//  3. 只有一个答案能通过，且能通过「逐候选计算签名」被正确判定；
//  4. 篡改与过期都必须失败。
func TestCaptcha(t *testing.T) {
	const secret = "test-secret"

	img, cookie, err := CaptchaImage(secret)
	if err != nil {
		t.Fatalf("生成验证码失败: %v", err)
	}
	if len(img) == 0 {
		t.Fatal("验证码图片是空的")
	}
	if _, err := png.Decode(bytes.NewReader(img)); err != nil {
		t.Fatalf("生成的图片不是合法 PNG: %v", err)
	}

	// 暴力找出正确答案（验证码答案域很小，这正是「不存明文」依然能校验的原因）。
	answers := []string{}
	for a := 0; a <= 18; a++ {
		if VerifyCaptcha(secret, cookie, strconv.Itoa(a)) {
			answers = append(answers, strconv.Itoa(a))
		}
	}
	if len(answers) != 1 {
		t.Fatalf("应当恰好有一个正确答案，实际 %v", answers)
	}

	if VerifyCaptcha(secret, cookie, "abc") {
		t.Fatal("非数字答案不应通过")
	}
	if VerifyCaptcha("wrong-secret", cookie, answers[0]) {
		t.Fatal("换了 secret 后不应通过")
	}
	if VerifyCaptcha(secret, cookie, answers[0]) == false {
		t.Fatal("正确答案应当通过")
	}

	// 篡改签名（把最后一位换掉）。
	parts := strings.Split(cookie, ".")
	tampered := strings.Join([]string{parts[0], parts[1], parts[2][:len(parts[2])-1] + "A"}, ".")
	if VerifyCaptcha(secret, tampered, answers[0]) {
		t.Fatal("签名被改后不应通过")
	}

	// 过期：直接构造一个已经过期的 cookie 值。
	expired := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) + "." + parts[1] + "." + parts[2]
	if VerifyCaptcha(secret, expired, answers[0]) {
		t.Fatal("过期后不应通过")
	}
}
