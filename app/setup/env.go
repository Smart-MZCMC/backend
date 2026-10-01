package setup

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 本文件只做一件事：以「保留原文件结构」的方式读写 .env。
//
// 为什么不用现成的 dotenv 库直接重写整个文件：.env.example 里有大量注释
// 说明（插件、在线更新、头像…），一次性重写会把它们全部抹掉，之后运维
// 想调插件配置时连字段含义都查不到。所以这里逐行解析，只替换命中的键，
// 其余内容原样保留。

// envKeyPattern 限定合法键名。带点的键也放行（历史遗留写法）。
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// EnvFile 是一份可读写的 .env。
type EnvFile struct {
	path  string
	lines []string
	eol   string
	index map[string]int
}

// LoadEnv 读取并解析 .env。
func LoadEnv(path string) (*EnvFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseEnv(path, string(data)), nil
}

// NewEnv 创建一个内存中的空 .env（尚未落盘）。
func NewEnv(path string) *EnvFile {
	return parseEnv(path, "")
}

// ParseEnvString 解析一段 .env 内容，用于测试。
func ParseEnvString(content string) *EnvFile {
	return parseEnv("", content)
}

func parseEnv(path, content string) *EnvFile {
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		// 原文件是 CRLF 就保持 CRLF，避免每次写入都产生一份巨大的 diff。
		eol = "\r\n"
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")

	f := &EnvFile{
		path:  path,
		lines: strings.Split(content, "\n"),
		eol:   eol,
		index: make(map[string]int),
	}
	for i, line := range f.lines {
		if key, _, ok := splitEnvLine(line); ok {
			// 重复键只认第一处，与 viper/godotenv 的「后者覆盖前者」不同，
			// 但 .env 里出现重复键本身就是错误，这里保证改的是能看到的那行。
			if _, exists := f.index[key]; !exists {
				f.index[key] = i
			}
		}
	}
	return f
}

// Path 返回该 .env 的路径。
func (f *EnvFile) Path() string { return f.path }

// Get 返回键对应的值；不存在返回空串。
func (f *EnvFile) Get(key string) string {
	i, ok := f.index[key]
	if !ok {
		return ""
	}
	_, value, _ := splitEnvLine(f.lines[i])
	return value
}

// Has 报告键是否存在（哪怕值为空）。
func (f *EnvFile) Has(key string) bool {
	_, ok := f.index[key]
	return ok
}

// Set 写入一个键。已存在则原地替换，否则追加到文件末尾。
//
// 追加前会吃掉末尾的空行——大多数 .env 以换行结尾，直接 append 会让新键
// 和上一行之间多出一个空行，多写几次就越来越散。
func (f *EnvFile) Set(key, value string) {
	line := key + "=" + encodeEnvValue(value)
	if i, ok := f.index[key]; ok {
		f.lines[i] = line
		return
	}
	for len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1]) == "" {
		f.lines = f.lines[:len(f.lines)-1]
	}
	f.index[key] = len(f.lines)
	f.lines = append(f.lines, line)
}

// Bytes 返回落盘内容。
func (f *EnvFile) Bytes() []byte {
	out := strings.Join(f.lines, f.eol)
	if out != "" && !strings.HasSuffix(out, f.eol) {
		out += f.eol
	}
	return []byte(out)
}

// Save 原子写入 .env。
//
// 先写临时文件再改名：初始化向导正在往这个文件里写 APP_KEY 与 JWT_SECRET，
// 写到一半断电/被杀会留下一个残缺的 .env，下次启动直接因为 APP_KEY 不合法
// 而拒绝启动。改名在同一分区上是原子的。
func (f *EnvFile) Save() error {
	if f.path == "" {
		return fmt.Errorf("未指定 .env 路径")
	}
	if dir := filepath.Dir(f.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmp := f.path + ".tmp"
	// .env 里有密钥，权限收到 0600（Windows 上该位不生效，由 ACL 决定）。
	if err := os.WriteFile(tmp, f.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// splitEnvLine 拆出一行的键值。注释行、空行、非法行返回 ok=false。
func splitEnvLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:eq])
	if !envKeyPattern.MatchString(key) {
		return "", "", false
	}
	return key, decodeEnvValue(strings.TrimSpace(trimmed[eq+1:])), true
}

// decodeEnvValue 解析值：支持单/双引号与行内注释，规则对齐 joho/godotenv
// （Goravel 用 viper 读 .env，viper 内部就是 godotenv）。
func decodeEnvValue(raw string) string {
	if raw == "" {
		return ""
	}

	switch raw[0] {
	case '"':
		// 找配对的结束引号：在第一个「未被转义」的引号处收尾。
		for i := 1; i < len(raw); i++ {
			if raw[i] != '"' {
				if raw[i] == '\\' {
					i++
				}
				continue
			}
			return unescapeDouble(raw[1:i])
		}
		return unescapeDouble(raw[1:])
	case '\'':
		if end := strings.IndexByte(raw[1:], '\''); end >= 0 {
			return raw[1 : 1+end]
		}
		return raw[1:]
	}

	// 未加引号：` #` 之后视为行内注释。
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = raw[:i]
	}
	if strings.HasPrefix(raw, "#") {
		return ""
	}
	return strings.TrimSpace(raw)
}

func unescapeDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// encodeEnvValue 决定一个值是否要加引号。
//
// 只有「加了引号才不会被 godotenv 解析错」的值才加：含空格、引号、反斜杠、
// 井号的，以及首尾有空白的。其余保持裸写，让 .env 保持可读。
func encodeEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if !strings.ContainsAny(value, " \t\"'#\\") && strings.TrimSpace(value) == value {
		return value
	}
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return `"` + replacer.Replace(value) + `"`
}

// 随机密钥用的字符集。不用 base64：`/`、`+`、`=` 在 .env 里是安全的，
// 但放进 URL 或 shell 里就要转义，没必要给自己找麻烦。
const randomAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// RandomString 生成 n 位随机字符串，失败时退化为固定串（不应发生）。
func RandomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 不可用等于系统熵源坏了，此时任何降级都可能被预测。
		// 返回空串会让调用方拒绝写盘，比静默写一个弱密钥安全。
		return ""
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = randomAlphabet[int(b)%len(randomAlphabet)]
	}
	return string(out)
}

// loadOrCreate 打开 .env；不存在时以 .env.example 为模板复制一份。
//
// 返回的 created 表示「这次新建了文件」，用于给用户提示。
func loadOrCreate(path, examplePath string) (f *EnvFile, created bool, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		f, err = LoadEnv(path)
		return f, false, err
	}

	if examplePath != "" {
		if data, readErr := os.ReadFile(examplePath); readErr == nil {
			return parseEnv(path, string(data)), true, nil
		}
	}
	return NewEnv(path), true, nil
}
