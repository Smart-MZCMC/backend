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

// extractBinary 从 .tar.gz 中取出唯一名为 binaryName 的可执行文件，写到 dest。
//
// 逐条路径都做了清洗：归档里的文件名是不可信输入，若直接 Join 到目标目录，
// 一个名为 "../../etc/cron.d/x" 的条目就能写到目标之外（zip slip）。
// 这里拒绝任何逃逸出目标目录的路径，并且只接受普通文件/目录，不处理符号链接
// 与设备节点——发布包里不需要它们，接受了反而是攻击面。
func extractBinary(archivePath, binaryName, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("不是合法的 gzip 文件: %w", err)
	}
	defer gz.Close()

	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	var entries int
	var total int64
	var out *os.File

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取归档失败: %w", err)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("归档条目数超过上限 %d，疑似异常包", maxEntries)
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("归档包含非预期的条目类型 %q，已拒绝", string(hdr.Typeflag))
		}

		clean, err := sanitizeEntryPath(hdr.Name)
		if err != nil {
			return err
		}

		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		total += hdr.Size
		if total > maxArtifactBytes {
			return fmt.Errorf("归档解压后超过上限 %d 字节，疑似压缩炸弹", int64(maxArtifactBytes))
		}

		if filepath.Base(clean) != binaryName {
			continue
		}
		if out != nil {
			// 已经解出一个了，又出现同名文件。
			//
			// 这里必须先关掉前一个：句柄不关的话，在 Windows 上测试清理临时目录
			// 会直接失败，在 Linux 上则一直泄漏到进程结束。
			out.Close()
			os.Remove(absDest)
			return fmt.Errorf("归档里有多个 %s，无法确定用哪个", binaryName)
		}

		if out, err = os.Create(absDest); err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxArtifactBytes)); err != nil {
			out.Close()
			os.Remove(absDest)
			return err
		}
	}

	if out == nil {
		return fmt.Errorf("归档包里没有找到 %s", binaryName)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(absDest, 0o755)
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
