//go:build !linux

package sysinfo

import "fmt"

// diskUsage 在非 linux 平台上返回「不支持」。
//
// 目标部署平台是 linux（release.yml 只交叉编译 linux/amd64 与 linux/arm64），
// 这里存在的意义是让开发机（Windows / macOS）上 `go build ./...` 与 `go vet ./...`
// 不会因为缺了 Statfs 就整包编译失败——那样本地根本没法验证，只能推到 CI 才发现。
//
// 刻意不按 BSD/darwin 再补一版 Statfs：那两个平台的 Statfs_t 字段名与 linux 有
// 差异（尤其 Bsize 的类型），为一台不会部署上去的机器维护一份没人验证过的实现，
// 风险比收益大。等真有需求时再加。
func diskUsage(path string) (fsUsage, error) {
	return fsUsage{}, fmt.Errorf("当前平台不支持读取文件系统用量")
}
