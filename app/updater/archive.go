package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxArtifactBytes 限制单个文件的解压后大小。
//
// 发布包解压后约 70MB（含三个静态站点），给到 512MB 的余量即可。
// 不设上限的话，一个构造过的压缩包（几 MB 压缩成几百 GB）就能把磁盘写满，
// 而这还是在没有校验和兜底的情况下。
const maxArtifactBytes = 512 << 20

// maxEntries 限制归档内的条目数，同样是为了挡住 zip bomb。
const maxEntries = 20000

// fileSHA256 返回 Reader 中全部内容的 sha256（十六进制小写）。
func fileSHA256(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyChecksum 在 checksums.txt 的内容里查找 asset 的期望摘要。
//
// checksums.txt 是 `sha256sum` 的输出格式：每行 "<64位十六进制>  <文件名>"，
// 文件名前可能带 * （表示二进制模式）。
func verifyChecksum(checksums []byte, asset, got string) error {
	want, ok := lookupChecksum(checksums, asset)
	if !ok {
		// 找不到条目就拒绝，而不是放行。校验环节「查不到」必须等同于「不通过」，
		// 否则一个被裁剪过的 checksums.txt 就能让校验形同虚设。
		return fmt.Errorf("checksums.txt 里没有 %s 的条目，无法校验完整性", asset)
	}
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("校验和不匹配：期望 %s，实际 %s", want, got)
	}
	return nil
}

func lookupChecksum(checksums []byte, asset string) (string, bool) {
	// 只要文件名部分完全相等，避免 "backend.tar.gz" 命中 "backend.tar.gz.old"
	want := filepath.Base(asset)
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == want {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// extractArchive 从 .tar.gz 中取出白名单内的条目，整份解到 destRoot 下，
// 返回实际解出了哪些白名单条目。
//
// 白名单而不是「把包里除运行期目录外的东西全解出来」：发布包里带着
// database/ 与 storage/ 的 .keep 占位，而这两个目录里是真实的数据库和日志。
// 按目录名 blanket 解压迟早会踩到它们。
//
// want 里的每一条同时按「精确路径」和「目录前缀」匹配：写 `public/admin` 就能
// 整份取出这个站点，写 `public/index.html` 就是取那个文件。两种解释并存不会
// 误伤——归档里不存在 `public/index.html/xxx` 这种路径。
//
// stripFirstComponent 剥掉归档最外层的那个目录（tools/package.go 打的包全部条目
// 都带一层 backend-linux-amd64/ 前缀），等价于 tar --strip-components=1。
//
// 同一个精确路径出现两次必须报错而不是后者覆盖前者：两个候选时无法确定用哪个，
// 这和旧实现对可执行文件的处理一致。
//
// 路径清洗与体积上限沿用旧的那套：归档里的文件名是不可信输入，符号链接与设备
// 节点一律拒绝，任何逃出 destRoot 的路径直接报错。
func extractArchive(archivePath, destRoot string, want []string, match func(rel string) bool) (map[string]bool, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("不是合法的 gzip 文件: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(destRoot)
	if err != nil {
		return nil, err
	}

	found := make(map[string]bool, len(want))
	written := make(map[string]bool, len(want))

	// 白名单一律转成系统分隔符再比。归档里的路径经过 sanitizeEntryPath 之后
	// 是系统形式（Windows 上是反斜杠），拿它去跟 "public/admin/" 这种写成
	// 斜杠的字面量比前缀，在 Windows 上永远不成立——于是整个站点白名单会被
	// 静默跳过，解出来只有一个可执行文件，正好回到这次要修的那个老毛病。
	wantOS := make([]string, len(want))
	origOf := make(map[string]string, len(want))
	for i, w := range want {
		wantOS[i] = filepath.FromSlash(w)
		origOf[wantOS[i]] = w
		found[w] = false
	}
	sep := string(filepath.Separator)

	tr := tar.NewReader(gz)
	var entries int
	var total int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		entries++
		if entries > maxEntries {
			return nil, fmt.Errorf("归档条目数超过上限 %d，疑似异常包", maxEntries)
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return nil, fmt.Errorf("归档包含非预期的条目类型 %q，已拒绝", string(hdr.Typeflag))
		}

		clean, err := sanitizeEntryPath(hdr.Name)
		if err != nil {
			return nil, err
		}
		rel := stripFirstComponent(clean)
		exact := false
		under := false
		for _, w := range wantOS {
			if rel == w {
				exact = true
			}
			if strings.HasPrefix(rel, w+sep) {
				under = true
			}
		}
		// 目录型目标只要底下有内容就算「解出来了」，不要求归档里带目录条目。
		// tar 加不加目录头取决于打包器（tools/package.go 用 filepath.Walk，
		// 恰好会写目录条目），把认领与否押在这上面太脆；而判错的后果是整个
		// 站点被跳过、前端永远不更新——正是这次要修的那个毛病。
		if exact || under {
			for _, w := range wantOS {
				if rel == w || strings.HasPrefix(rel, w+sep) {
					found[origOf[w]] = true
				}
			}
		}

		if hdr.Typeflag == tar.TypeDir {
			// 目录条目不用写盘：真正写文件时会 MkdirAll。
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !exact && !under {
			continue
		}
		if match != nil && !match(rel) {
			continue
		}
		if exact {
			if written[rel] {
				return nil, fmt.Errorf("归档里有多个 %s，无法确定用哪个", rel)
			}
			written[rel] = true
		}

		// 再兜一次 dest：解出的路径必须仍在 destRoot 之内。
		// sanitizeEntryPath 已经挡掉了 ..，这里是防止将来匹配逻辑改动时把
		// 「相对根目录」当成「相对归档根目录」而静默写到别处。
		outPath := filepath.Join(absRoot, rel)
		if outPath != absRoot && !strings.HasPrefix(outPath, absRoot+sep) {
			return nil, fmt.Errorf("归档条目解出后越界: %q", hdr.Name)
		}

		total += hdr.Size
		if total > maxArtifactBytes {
			return nil, fmt.Errorf("归档解压后超过上限 %d 字节，疑似压缩炸弹", int64(maxArtifactBytes))
		}

		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return nil, err
		}
		out, err := os.Create(outPath)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxArtifactBytes)); err != nil {
			out.Close()
			os.Remove(outPath)
			return nil, err
		}
		if err := out.Close(); err != nil {
			os.Remove(outPath)
			return nil, err
		}
		if exact {
			found[origOf[rel]] = true
		}
	}

	return found, nil
}

// stripFirstComponent 去掉归档最外层目录。
//
// 路径只剩一个组件时原样返回：单文件的归档（例如测试夹具）不该被剥成空串。
func stripFirstComponent(clean string) string {
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if len(parts) < 2 {
		return clean
	}
	return filepath.FromSlash(strings.Join(parts[1:], "/"))
}

// sanitizeEntryPath 清洗归档内的相对路径。
func sanitizeEntryPath(name string) (string, error) {
	// Windows 上归档里可能是反斜杠，统一成斜杠再判断。
	name = strings.ReplaceAll(name, `\`, "/")
	// 归档应使用相对路径且不含盘符。绝对路径一律拒绝。
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("归档包含绝对路径 %q，已拒绝", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("归档包含越界路径 %q，已拒绝", name)
	}
	return clean, nil
}

// elfFingerprint 读取可执行文件的 ELF 架构标识，用于确认拿到的确实是
// 目标平台的二进制，而不是一个网页错误页或 HTML 标签页。
//
// 只看 magic 与 e_machine 两个字段，足以区分 amd64 / arm64 / 非 ELF。
func elfFingerprint(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(b) < 20 {
		return "", errors.New("文件太小，不是可执行文件")
	}
	if !bytes.Equal(b[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return "", errors.New("不是 ELF 文件（可能下载到了错误页面而不是发布包）")
	}
	// e_machine 是小端 uint16，位于偏移 18。
	machine := uint16(b[18]) | uint16(b[19])<<8
	switch machine {
	case 62:
		return "amd64", nil
	case 183:
		return "arm64", nil
	default:
		return fmt.Sprintf("machine=%d", machine), nil
	}
}
