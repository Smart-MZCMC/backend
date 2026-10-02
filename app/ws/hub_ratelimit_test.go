package ws

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 这条规则防的是后端进程整体消失。
//
// 速率限制此前是个没有锁的包级 map，而它写在每一条 WebSocket 连接的处理
// 路径上。多端同时重连时并发写 map 触发的是 runtime 的 fatal error
// （不是 panic，recover 无效），现场表现是播到一半所有客户端一起断线、
// 服务端进程没了，只留一行 maps.fatal。
//
// 修复前这个用例跑起来会直接带崩测试进程，所以它既是回归测试也是当时的
// 复现脚本。-race 下能顺带覆盖读侧。
func TestAllowConnection_同一标识一秒内只放行一次(t *testing.T) {
	const goroutines = 64

	// 先清掉上一次运行留下的状态，避免受测试执行顺序影响。
	connectionRateLimit.mu.Lock()
	connectionRateLimit.seen = make(map[string]time.Time)
	connectionRateLimit.mu.Unlock()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 让所有 goroutine 同时冲进临界区，最大化并发窗口
			if allowConnection("1:director:") {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 1 {
		t.Fatalf("同一标识 1 秒内应只放行 1 次，实际放行 %d 次", got)
	}
}

// 这条规则防的是内存单调增长。
//
// 键是 (project_id, role, point_code) 组合，项目和采访点一多就会一直累积，
// 而这个 map 常驻进程内、没有任何人负责回收——长时间挂着的部署会把它
// 慢慢吃满。这里验证过期项确实会被清掉。
func TestAllowConnection_过期标识会被回收(t *testing.T) {
	connectionRateLimit.mu.Lock()
	connectionRateLimit.seen = make(map[string]time.Time)
	connectionRateLimit.mu.Unlock()

	for _, key := range []string{"1:director:", "1:commentator:", "1:packaging:"} {
		if !allowConnection(key) {
			t.Fatalf("首次连接 %s 应放行", key)
		}
	}

	connectionRateLimit.mu.Lock()
	held := len(connectionRateLimit.seen)
	connectionRateLimit.mu.Unlock()
	if held != 3 {
		t.Fatalf("窗口内应保留 3 个标识，实际 %d 个", held)
	}

	// 等过整个窗口，让下一次调用把这些项当作过期项处理。
	time.Sleep(1100 * time.Millisecond)

	if !allowConnection("1:director:") {
		t.Fatal("超过 1 秒后应重新放行")
	}

	connectionRateLimit.mu.Lock()
	held = len(connectionRateLimit.seen)
	connectionRateLimit.mu.Unlock()
	if held != 1 {
		t.Fatalf("过期标识应被回收，只剩当前这一个，实际剩 %d 个", held)
	}
}
