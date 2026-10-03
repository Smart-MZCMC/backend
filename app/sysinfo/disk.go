package sysinfo

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// maxDirEntries 统计目录大小时最多下探多少个条目。
//
// 这是给「读一遍目录」这件事上的一个硬上限：监控接口是不该被一个异常目录
// （比如误挂了一个网络盘、或者某个目录里有几百万个文件）拖住甚至拖挂的。
// 超过上限就返回已统计到的部分，并在 Detail 里说明——宁可给一个偏小的数字，
// 也不要让监控页自己超时。
const maxDirEntries = 20000

// collectDisk 统计承载数据文件的文件系统用量，以及若干应用目录各自的体积。
func collectDisk(dataDir string, appDirs map[string]string) Disk {
	d := Disk{Path: dataDir, Supported: true}

	usage, err := diskUsage(dataDir)
	if err != nil {
		d.Supported = false
		d.Note = fmt.Sprintf("拿不到 %s 的磁盘用量：%v", dataDir, err)
	} else {
		d.TotalBytes = usage.total
		d.FreeBytes = usage.free
		d.UsedBytes = usage.used()
		d.UsedPercent = usage.usedPercent()
	}

	names := make([]string, 0, len(appDirs))
	for name := range appDirs {
		names = append(names, name)
	}
	// 排序是为了让返回顺序稳定：Go 的 map 遍历是随机的，不排序的话同一份数据
	// 每次刷新出来的条目顺序都在变，前端的 diff 和用户的观察都会失去意义。
	sort.Strings(names)

	for _, name := range names {
		bytes, err := dirSize(appDirs[name])
		entry := DirSize{Name: name, Bytes: bytes}
		if err != nil {
			entry.Error = err.Error()
		}
		d.AppBytesDetail = append(d.AppBytesDetail, entry)
		d.AppBytes += bytes
	}

	return d
}

// dirSize 递归累加目录里所有普通文件的字节数。
func dirSize(root string) (uint64, error) {
	var total uint64
	var count int
	stopped := false

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// 单个条目读不出来（权限、已被删除的符号链接）不该让整次统计失败，
			// 记一笔继续走。真正要报的是「这个目录整体不可访问」。
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > maxDirEntries {
			stopped = true
			return filepath.SkipAll
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			// 符号链接、设备节点一律跳过：跟统计发布包时同一个理由，
			// 它们不该被算进「这个目录占了多少空间」。
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += uint64(info.Size())
		return nil
	})

	if stopped {
		return total, fmt.Errorf("条目超过 %d 个，只统计了前一部分", maxDirEntries)
	}
	if err != nil {
		return total, err
	}
	if _, statErr := os.Stat(root); statErr != nil {
		return total, statErr
	}
	return total, nil
}

// fsUsage 是某次统计得到的文件系统用量。
type fsUsage struct {
	total uint64
	free  uint64
}

func (u fsUsage) used() uint64 {
	if u.total < u.free {
		return 0
	}
	return u.total - u.free
}

func (u fsUsage) usedPercent() float64 {
	if u.total == 0 {
		return 0
	}
	return float64(u.used()) / float64(u.total) * 100
}
