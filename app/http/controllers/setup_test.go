package controllers

import (
	"strings"
	"testing"
)

func TestSetupHostForURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1", "127.0.0.1"},
		{"example.com", "example.com"},
		{"192.168.1.10", "192.168.1.10"},
	}
	for _, c := range cases {
		if got := setupHostForURL(c.in); got != c.want {
			t.Errorf("setupHostForURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// 0.0.0.0 是「监听全部网卡」，直接拼进 URL 在部分系统上打不开，
	// 必须换成具体地址。
	for _, host := range []string{"0.0.0.0", "::", ""} {
		got := setupHostForURL(host)
		if got == "0.0.0.0" || got == "::" || got == "" {
			t.Errorf("setupHostForURL(%q) = %q，应替换为可用主机", host, got)
		}
	}
}

func TestSetupWsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:3000", "ws://127.0.0.1:3000/ws"},
		{"https://zhdb.example.com", "wss://zhdb.example.com/ws"},
		{"http://192.168.1.10:8080", "ws://192.168.1.10:8080/ws"},
		{"", ""},
	}
	for _, c := range cases {
		if got := setupWsURL(c.in); got != c.want {
			t.Errorf("setupWsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]string{"APP_PORT": "3000", "APP_NAME": "x", "APP_HOST": "0.0.0.0"})
	want := "APP_HOST,APP_NAME,APP_PORT"
	if strings.Join(got, ",") != want {
		t.Errorf("sortedKeys = %v, want %s", got, want)
	}
	if len(sortedKeys(nil)) != 0 {
		t.Error("空 map 应返回空切片")
	}
}
