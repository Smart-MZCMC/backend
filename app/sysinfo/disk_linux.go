//go:build linux

package sysinfo

import "syscall"

// diskUsage 用 statfs 读文件系统的块数。
//
// 为什么自己写而不用 gopsutil：现场服务器未必有外网，拉依赖很可能直接失败；
// 而这一段就是十几行 syscall，审得过来。statfs 在 linux 上是最便宜的做法——
// 它读的是内核里已缓存的超级块信息，不扫目录。
func diskUsage(path string) (fsUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsUsage{}, err
	}
	bsize := uint64(st.Bsize)
	return fsUsage{
		// Blocks 是全部块，含保留给 root 的那部分；Bavail 是普通用户可用的。
		// 「已用」按 total - free 算，其中 free 用 Bavail，与 df 的输出口径一致。
		total: st.Blocks * bsize,
		free:  st.Bavail * bsize,
	}, nil
}
