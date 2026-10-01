package models

import (
	"strings"
	"testing"
)

// 邮箱归一化必须与写入路径一致：唯一约束和头像哈希都基于归一化后的值。
// 两者一旦用不同的值，就会出现「A@x.com 与 a@x.com 是两条记录却共用一张
// 头像」，或者「明明只有一个邮箱却提示已被占用」。
func TestNormalizeEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"test@example.com", "test@example.com"},
		{"  test@example.com  ", "test@example.com"},
		{"TEST@EXAMPLE.COM", "test@example.com"},
		{"  TeSt@Example.Com\t", "test@example.com"},
		{"test+tag@example.com", "test+tag@example.com"},
		{"中文@例子.中国", "中文@例子.中国"},
	}
	for _, c := range cases {
		if got := NormalizeEmail(c.in); got != c.want {
			t.Errorf("NormalizeEmail(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEmail_IsIdempotent(t *testing.T) {
	// 归一化后再归一化必须不变，否则每次读改写都会让 email 漂移，
	// 唯一索引也会跟着抖动。
	once := NormalizeEmail("  Mixed.Case@Example.COM ")
	twice := NormalizeEmail(once)
	if once != twice {
		t.Fatalf("非幂等：%q -> %q", once, twice)
	}
}

func TestValidEmail(t *testing.T) {
	valid := []string{
		"a@b.co",
		"test@example.com",
		"first.last+tag@sub.example.co.uk",
		"数字@例子.cn",
		"x@y-z.com",
	}
	for _, e := range valid {
		if !ValidEmail(e) {
			t.Errorf("ValidEmail(%q) 应为 true", e)
		}
	}

	invalid := []string{
		"",
		"no-at-sign",
		"@example.com",
		"user@",
		"user@example",
		"user@.com",
		"user@example.",
		"user@exam ple.com", // 含空格
		"a@b@c.com",         // 两个 @
		strings.Repeat("x", 250) + "@example.com", // 超长
	}
	for _, e := range invalid {
		if ValidEmail(e) {
			t.Errorf("ValidEmail(%q) 应为 false", e)
		}
	}
}

// 头像 URL 的算法必须与 WeAvatar（Gravatar 系）一致：MD5(去空格后的小写邮箱)。
// 算错不会报错，只会静默返回默认头像，所以这个值要钉死。
func TestAvatarURL(t *testing.T) {
	// 这是 md5("test@example.com") 的公认值，可独立验证而不依赖本实现。
	const knownMD5 = "55502f40dc8b7c769880b10874abc9d0"

	u := User{Email: "test@example.com"}
	got := u.AvatarURL()
	if !strings.Contains(got, knownMD5) {
		t.Fatalf("头像 URL 应含 md5=%s，实际 %s", knownMD5, got)
	}
	if !strings.HasPrefix(got, "https://weavatar.com/avatar/") {
		t.Errorf("头像 URL 前缀不对：%s", got)
	}
	if !strings.Contains(got, "?s=") {
		t.Errorf("头像 URL 应带尺寸参数：%s", got)
	}

	// 大小写与空格必须归一化后再哈希，否则同一个人换种写法就会换张头像。
	if other := (User{Email: "  TEST@Example.com  "}).AvatarURL(); other != got {
		t.Errorf("大小写/空格不同却算出不同头像：\n  %s\n  %s", got, other)
	}
}

func TestAvatarURL_EmptyEmail(t *testing.T) {
	// 空邮箱必须返回空串，让前端回退到首字母圆圈而不是去请求一个
	// 无意义的 URL（md5("") 也能算出值，但那样每个没填邮箱的用户都会
	// 拿到同一张默认头像，看起来像"所有人共用一个头像"）。
	if got := (User{}).AvatarURL(); got != "" {
		t.Errorf("无邮箱时头像 URL 应为空，实际 %q", got)
	}
	if got := (User{Email: "   "}).AvatarURL(); got != "" {
		t.Errorf("空白邮箱时头像 URL 应为空，实际 %q", got)
	}
}
