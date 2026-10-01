package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFileGet(t *testing.T) {
	content := strings.Join([]string{
		"# 注释行",
		"",
		"APP_NAME=Goravel",
		"APP_URL=\"http://127.0.0.1:3000\"",
		"JWT_SECRET=abc123",
		"WITH_SPACE=hello world",
		"INLINE=value # 这是注释",
		"SINGLE='it is fine'",
		"EMPTY=",
		"# DB_HOST=commented",
	}, "\n")

	f := parseEnv("", content)

	cases := map[string]string{
		"APP_NAME":    "Goravel",
		"APP_URL":     "http://127.0.0.1:3000",
		"JWT_SECRET":  "abc123",
		"WITH_SPACE":  "hello world",
		"INLINE":      "value",
		"SINGLE":      "it is fine",
		"EMPTY":       "",
		"DB_HOST":     "", // 注释掉的键不能被读到
		"NOT_PRESENT": "",
	}
	for key, want := range cases {
		if got := f.Get(key); got != want {
			t.Errorf("Get(%q) = %q, want %q", key, got, want)
		}
	}

	if f.Has("DB_HOST") {
		t.Error("注释行不应被当成配置项")
	}
	if !f.Has("EMPTY") {
		t.Error("值为空的键仍然存在，Has 应为 true")
	}
}

func TestEnvFileSetReplacesInPlaceAndKeepsComments(t *testing.T) {
	content := "# 头部注释\nAPP_NAME=Goravel\nJWT_SECRET=old\n"
	f := parseEnv("", content)

	f.Set("APP_NAME", "绵中融媒体智汇导播系统")
	f.Set("NEW_KEY", "new-value")

	out := string(f.Bytes())

	if !strings.Contains(out, "# 头部注释") {
		t.Error("写入后注释丢失")
	}
	if !strings.Contains(out, "APP_NAME=绵中融媒体智汇导播系统") {
		t.Errorf("APP_NAME 未被替换，当前内容：\n%s", out)
	}
	if !strings.Contains(out, "JWT_SECRET=old") {
		t.Error("未改动的键被破坏")
	}
	if !strings.Contains(out, "NEW_KEY=new-value") {
		t.Error("新键未追加")
	}
	// 原键的行序不变：APP_NAME 仍应排在 JWT_SECRET 之前。
	if strings.Index(out, "APP_NAME") > strings.Index(out, "JWT_SECRET") {
		t.Error("替换键时改变了原有行序")
	}
}

func TestEnvFileSetReusesExistingKey(t *testing.T) {
	f := parseEnv("", "A=1\n")
	f.Set("A", "2")
	f.Set("A", "3")

	if got := f.Get("A"); got != "3" {
		t.Errorf("重复 Set 后 Get = %q, want 3", got)
	}
	if n := strings.Count(string(f.Bytes()), "A="); n != 1 {
		t.Errorf("重复 Set 生成了 %d 行 A=，want 1", n)
	}
}

func TestEnvValueRoundTrip(t *testing.T) {
	values := []string{
		"plain",
		"with space",
		"中文 带空格",
		"has#hash",
		`quote"inside`,
		`back\slash`,
		"tab\there",
		"#leading-hash",
		"http://127.0.0.1:3000",
	}
	for _, want := range values {
		f := parseEnv("", "KEY=x\n")
		f.Set("KEY", want)

		parsed := parseEnv("", string(f.Bytes()))
		if got := parsed.Get("KEY"); got != want {
			t.Errorf("round trip %q -> %q（落盘内容：%s）", want, got, f.Bytes())
		}
	}
}

func TestEnvFilePreservesCRLF(t *testing.T) {
	f := parseEnv("", "A=1\r\nB=2\r\n")
	if f.eol != "\r\n" {
		t.Fatalf("eol = %q, want CRLF", f.eol)
	}
	f.Set("C", "3")
	if !strings.Contains(string(f.Bytes()), "C=3\r\n") {
		t.Errorf("新键未使用 CRLF：%q", string(f.Bytes()))
	}
}

func TestSetRemovesTrailingBlankLinesBeforeAppend(t *testing.T) {
	f := parseEnv("", "A=1\n\n")
	f.Set("B", "2")
	out := string(f.Bytes())
	if strings.Contains(out, "\n\n") {
		t.Errorf("追加后出现多余空行：%q", out)
	}
	if !strings.HasSuffix(out, "A=1\nB=2\n") {
		t.Errorf("落盘内容不符合预期：%q", out)
	}
}

func TestLoadOrCreateFallsBackToExample(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, ".env.example")
	if err := os.WriteFile(example, []byte("APP_NAME=FromExample\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, ".env")
	f, created, err := loadOrCreate(path, example)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("created 应为 true")
	}
	if got := f.Get("APP_NAME"); got != "FromExample" {
		t.Errorf("未以模板为初值，APP_NAME = %q", got)
	}
}

func TestEnsureSecretsFillsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# 注释\nAPP_NAME=MyApp\nAPP_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filled, created, err := ensureSecrets(path, filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("已存在的 .env 不应被标记为新建")
	}

	want := map[string]bool{"APP_KEY": true, "JWT_SECRET": true, "DB_CONNECTION": true, "DB_DATABASE": true}
	if len(filled) != len(want) {
		t.Fatalf("补齐的键 = %v，want %v", filled, want)
	}
	for _, key := range filled {
		if !want[key] {
			t.Errorf("不应补齐 %s", key)
		}
	}

	f, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Get("APP_KEY")) != SecretLength {
		t.Errorf("APP_KEY 长度 = %d, want %d", len(f.Get("APP_KEY")), SecretLength)
	}
	if len(f.Get("JWT_SECRET")) != SecretLength {
		t.Errorf("JWT_SECRET 长度 = %d, want %d", len(f.Get("JWT_SECRET")), SecretLength)
	}
	if f.Get("DB_CONNECTION") != "sqlite" {
		t.Errorf("DB_CONNECTION = %q, want sqlite", f.Get("DB_CONNECTION"))
	}
	// 用户已有内容必须原样保留。
	if f.Get("APP_NAME") != "MyApp" {
		t.Errorf("APP_NAME 被改动：%q", f.Get("APP_NAME"))
	}
	if !strings.Contains(string(f.Bytes()), "# 注释") {
		t.Error("注释丢失")
	}
}

func TestEnsureSecretsKeepsExistingSecretsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "APP_KEY=01234567890123456789012345678901\nJWT_SECRET=fixed-secret\nDB_CONNECTION=sqlite\nDB_DATABASE=database/x.db\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	filled, _, err := ensureSecrets(path, filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if len(filled) != 0 {
		t.Errorf("配置已齐全时不应改动任何键，实际补齐: %v", filled)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Errorf("内容被改写：\n%s", after)
	}
}

func TestEnsureSecretsRepairsNonSQLiteConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DB_CONNECTION=postgres\nAPP_KEY=01234567890123456789012345678901\nJWT_SECRET=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	filled, _, err := ensureSecrets(path, filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(filled, ",") != "DB_CONNECTION,DB_DATABASE" {
		t.Errorf("补齐的键 = %v, want [DB_CONNECTION DB_DATABASE]", filled)
	}

	f, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Get("DB_CONNECTION") != "sqlite" {
		t.Errorf("DB_CONNECTION = %q, want sqlite", f.Get("DB_CONNECTION"))
	}
}

func TestRandomStringLengthAndCharset(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := RandomString(32)
		if len(s) != 32 {
			t.Fatalf("长度 = %d, want 32", len(s))
		}
		for _, r := range s {
			if !strings.ContainsRune(randomAlphabet, r) {
				t.Fatalf("出现非法字符 %q", r)
			}
		}
	}
	if RandomString(32) == RandomString(32) {
		t.Error("两次生成的密钥相同，随机源可能有问题")
	}
}

func TestWriteAppConfigSkipsEmptyValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("APP_NAME=Old\nAPP_HOST=127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	written, err := writeAppConfig(path, filepath.Join(dir, ".env.example"), []EnvValue{
		{Key: "APP_NAME", Value: "New"},
		{Key: "APP_HOST", Value: ""},
		{Key: "APP_PORT", Value: "3000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || written["APP_NAME"] != "New" || written["APP_PORT"] != "3000" {
		t.Errorf("写入结果 = %v, want APP_NAME 与 APP_PORT", written)
	}

	f, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Get("APP_HOST") != "127.0.0.1" {
		t.Errorf("空值不应清空已有键，APP_HOST = %q", f.Get("APP_HOST"))
	}
	if f.Get("APP_PORT") != "3000" {
		t.Errorf("新键未写入，APP_PORT = %q", f.Get("APP_PORT"))
	}
}
