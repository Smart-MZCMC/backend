// Package main 集成测试脚本
// 模拟完整的导播协调流程：初始化 → 登录 → 建账号 → 建项目 → 授权 →
// WebSocket 连接 → 切台 → 采访状态 → 切台报表 → 日志查询与导出 → 清理与审计。
//
// 用法: go run tests/integration/main.go [base_url]（默认 http://127.0.0.1:3000）
//
// 脚本会自己把实例拉到可用状态：全新库走一次初始化向导，已初始化过就直接登录。
// 因此对空库和已跑过的库都能运行。注意它会**真的建项目、建账号、写审计**，
// 只应指向临时实例，不要指向正式环境。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

var (
	baseURL    = "http://127.0.0.1:3000"
	wsURL      = "ws://127.0.0.1:3002/ws"
	httpClient = &http.Client{Timeout: 10 * time.Second}
)

func main() {
	if len(os.Args) > 1 {
		baseURL = os.Args[1]
	}

	fmt.Println("========================================")
	fmt.Println("  校园直播导播协调系统 - 集成测试")
	fmt.Println("========================================")
	fmt.Println()

	passed := 0
	failed := 0

	tests := []struct {
		name string
		fn   func() error
	}{
		{"1. 服务器状态检查", testServerStatus},
		{"2. 确保系统已初始化", testEnsureInitialized},
		{"3. 管理员登录", testLoginAdmin},
		{"4. 创建导播账号", testCreateDirector},
		{"5. 创建项目", testCreateProject},
		{"6. 分配项目权限", testAssignProject},
		{"7. WebSocket连接(导播A)", testWSDirectorA},
		{"8. WebSocket连接(导播B)", testWSDirectorB},
		{"9. WebSocket连接(采访端)", testWSInterviewer},
		{"10. 采访状态推送", testInterviewStatus},
		{"11. 获取控制权", testAcquireLock},
		{"12. 心跳续期", testHeartbeat},
		{"13. 释放控制权", testReleaseLock},
		{"14. 导播B切台(shot_state)", testConfirmSwitch},
		{"15. 切台报表", testShotCuts},
		{"16. 日志查询", testLogsQuery},
		{"17. 日志导出(JSON)", testExportJSON},
		{"18. 日志导出(CSV)", testExportCSV},
		{"19. 插件列表", testPluginList},
		{"20. 手动清理日志", testCleanupLogs},
		{"21. 清理留下审计记录", testCleanupAudited},
		{"22. 项目统计", testProjectStats},
	}

	for _, t := range tests {
		fmt.Printf("  %-40s", t.name)
		if err := t.fn(); err != nil {
			fmt.Printf("FAIL\n    → %v\n", err)
			failed++
		} else {
			fmt.Println("OK")
			passed++
		}
		time.Sleep(1200 * time.Millisecond) // 请求间隔
	}

	fmt.Println()
	fmt.Printf("  结果: %d 通过, %d 失败, 共 %d\n", passed, failed, passed+failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// --- 工具函数 ---

func post(path string, body any) (int, map[string]any, error) {
	return postAuth(path, body, "")
}

// postAuth 带令牌的 POST。
//
// 必须有这个变体：1.1.0 给管理接口加了 RequireRole 之后，不带 Authorization
// 的调用一律 401，而当时这份脚本没跟着改 —— 它从这里开始整份失效，
// 包括「注册」「创建项目」这些看着最基础的一步。加鉴权是好改动，
// 但验证脚本没跟上，等于这项验证悄悄停了。
func postAuth(path string, body any, token string) (int, map[string]any, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(b, &result)
	return resp.StatusCode, result, nil
}

func get(path string, token string) (int, any, error) {
	req, _ := http.NewRequest("GET", baseURL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result any
	json.Unmarshal(b, &result)
	return resp.StatusCode, result, nil
}

func put(path string, body any, token string) (int, map[string]any, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", baseURL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(b, &result)
	return resp.StatusCode, result, nil
}

func del(path string, token string) (int, map[string]any, error) {
	req, _ := http.NewRequest("DELETE", baseURL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(b, &result)
	return resp.StatusCode, result, nil
}

// --- 测试变量 ---
const (
	adminUsername = "test_admin"
	adminPassword = "123456"
	directorName  = "test_director"
	projectCode   = "test_sports_2026"
)

var (
	adminToken  string
	projectID   float64
	directorAID float64
	directorBID float64
)

// --- 测试用例 ---

// testServerStatus 查服务状态。
//
// 全新部署时整个 API 都返回 503（初始化模式），这不是故障，下一站会把它
// 拉起来，所以这里把 503 也认作「服务在跑」。
func testServerStatus() error {
	code, body, err := get("/api/status", "")
	if err != nil {
		return err
	}
	if code == 503 {
		fmt.Print("[需要初始化] ")
		return nil
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d, 期望 200 或 503", code)
	}
	b, ok := body.(map[string]any)
	if !ok || b["status"] != "running" {
		return fmt.Errorf("状态: %v", body)
	}
	if v, _ := b["version"].(string); v != "" {
		fmt.Printf("[v%s] ", v)
	}
	return nil
}

// testEnsureInitialized 让脚本自己把实例拉到可用状态。
//
// 全新库时走初始化向导建出管理员账号；已经初始化过就什么都不做，
// 下一步直接登录。这样脚本既能对空库跑，也能重复跑。
//
// 之前这一步是「调 /api/auth/register 抢注管理员」——而全新的库处于初始化
// 模式，所有 API 都是 503，抢注根本不可能成功；库非空时该接口又（正确地）
// 需要管理员令牌。两条路都走不通，于是这份脚本长期全部失败。
func testEnsureInitialized() error {
	code, body, err := get("/api/setup/status", "")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("查询初始化状态失败，状态码 %d", code)
	}
	m, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("初始化状态返回格式错误: %v", body)
	}
	if m["needs_setup"] != true {
		return nil // 已经初始化过，直接去登录
	}

	c, b, err := post("/api/setup/apply", map[string]any{
		"app_name":           "集成测试实例",
		"app_url":            baseURL,
		"admin_username":     adminUsername,
		"admin_password":     adminPassword,
		"admin_display_name": "测试管理员",
	})
	if err != nil {
		return err
	}
	if c != 200 && c != 201 {
		return fmt.Errorf("初始化失败，状态码 %d，响应 %v", c, b)
	}
	fmt.Print("[已初始化] ")
	return nil
}

func testLoginAdmin() error {
	code, body, err := post("/api/auth/login", map[string]any{
		"username": adminUsername,
		"password": adminPassword,
	})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d, 期望 200", code)
	}
	token, ok := body["token"].(string)
	if !ok || token == "" {
		return fmt.Errorf("未获取到 token")
	}
	adminToken = token
	return nil
}

// testCreateDirector 用管理员令牌建一个导播账号。
//
// 走的是 /api/auth/register 的非引导模式（已有用户时必须带管理员令牌），
// 比抢注第一个账号更接近日常路径。
func testCreateDirector() error {
	code, body, err := postAuth("/api/auth/register", map[string]any{
		"username":     directorName,
		"password":     "123456",
		"display_name": "测试导播",
		"role":         "director",
	}, adminToken)
	if err != nil {
		return err
	}
	if code != 201 && code != 409 {
		return fmt.Errorf("状态码 %d, 期望 201/409，响应 %v", code, body)
	}
	if id, ok := body["id"].(float64); ok {
		directorAID = id
	}
	// 409（已存在）时按用户名查一次 ID，供后面的授权步骤使用。
	if directorAID == 0 {
		code, list, err := get("/api/admin/users", adminToken)
		if err != nil || code != 200 {
			return fmt.Errorf("查询用户列表失败，状态码 %d", code)
		}
		for _, item := range list.([]any) {
			u, _ := item.(map[string]any)
			if u["username"] == directorName {
				directorAID, _ = u["id"].(float64)
			}
		}
	}
	if directorAID == 0 {
		return fmt.Errorf("没能确定导播账号的 ID")
	}
	return nil
}

func testAssignProject() error {
	code, body, err := postAuth("/api/admin/assign", map[string]any{
		"user_id":    int(directorAID),
		"project_id": int(projectID),
	}, adminToken)
	if err != nil {
		return err
	}
	// 已经授权过会返回 409，同样算通过。
	if code != 201 && code != 409 {
		return fmt.Errorf("状态码 %d, 期望 201/409，响应 %v", code, body)
	}
	return nil
}

func testCreateProject() error {
	code, body, err := postAuth("/api/admin/projects", map[string]any{
		"name":            "运动会测试项目",
		"code":            projectCode,
		"description":     "集成测试用项目",
		"venue":           "田径场",
		"scheduled_start": "2026-05-20T09:00",
		"scheduled_end":   "2026-05-20T18:00",
		"status":          "planned",
		"mode":            "live",
	}, adminToken)
	if err != nil {
		return err
	}
	if code != 201 && code != 409 {
		return fmt.Errorf("状态码 %d, 期望 201/409", code)
	}
	if id, ok := body["id"].(float64); ok {
		projectID = id
	}
	// 409（编码已存在）时响应里没有 id，按编码回查一次。
	// 少了这一步，脚本第二次跑就会因为 projectID=0 在后面全线失败——
	// 「只能跑一次」的验证脚本和不能跑没什么区别。
	if projectID == 0 {
		code, list, err := get("/api/admin/projects", adminToken)
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("查询项目列表失败，状态码 %d", code)
		}
		items, _ := list.([]any)
		for _, item := range items {
			p, _ := item.(map[string]any)
			if p["code"] == projectCode {
				projectID, _ = p["id"].(float64)
			}
		}
	}
	if projectID == 0 {
		return fmt.Errorf("没能确定项目 %s 的 ID", projectCode)
	}
	return nil
}

func testWSDirectorA() error {
	return connectWS("director", int(projectID), adminToken)
}

// testWSDirectorB 第二个导播连接同一项目。
//
// 控制权已被 A 持有，但**连接本身是允许的**（导播端需要连上才能看到锁状态
// 并等待接管），所以这里只验证连接能建立。
// 这个用例原来叫「导播B-被拒」并期望握手失败——实际上它失败的原因是连接
// 限流（同一 project+role 1 秒内不许重复连），用例名和断言都在指错方向。
func testWSDirectorB() error {
	return connectWS("director", int(projectID), adminToken)
}

func testWSInterviewer() error {
	return connectWS("interviewer", int(projectID), "")
}

func testInterviewStatus() error {
	// 采访状态上报是公开接口（采访端是 Web 产物，走的是匿名通道）。
	code, _, err := post("/api/interview/status", map[string]any{
		"project_id": projectID,
		"point_code": "point_1",
		"point_name": "测试采访点",
		"status":     "preparing",
	})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d, 期望 200", code)
	}
	return nil
}

// testReleaseLock / testAcquireLock 走带令牌的路径。
//
// 原来先发一次无令牌请求、再靠 err 判断要不要重试，而 401 是**正常的 HTTP
// 响应**、不是 err，于是那条重试分支从来不会走到，测试实际测的是 401。
//
// 顺序上有个容易误判的地方：第 7/8 步的导播 WebSocket 在**断开时**会自动释放
// 控制权（`releaseLockOnDisconnect`），所以等跑到这里时锁已经没了。因此这几步
// 按「获取 → 心跳 → 释放」自己走一遍完整周期，而不是假设前一步的连接还持有锁。
func testHeartbeat() error {
	return postExpectOK("/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/heartbeat", "心跳续期")
}

func testReleaseLock() error {
	return postExpectOK("/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/release", "释放控制权")
}

func testAcquireLock() error {
	return postExpectOK("/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/acquire", "获取控制权")
}

// postExpectOK 发一个带管理员令牌的空体 POST，要求 200。
func postExpectOK(path, label string) error {
	code, body, err := postAuth(path, map[string]any{}, adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("%s失败，状态码 %d，响应 %v", label, code, body)
	}
	return nil
}

func testConfirmSwitch() error {
	// 走现行协议 shot_state。confirm_switch / next_shot 已在协议升级时废弃，
	// 后端收到会回一条「请刷新客户端」并丢弃——继续用旧协议等于这条用例
	// 根本没验证切台链路。
	msg := map[string]any{
		"type":       "shot_state",
		"project_id": projectID,
		"payload":    map[string]any{"current": "全景", "next": ""},
	}
	data, _ := json.Marshal(msg)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"?project_id="+strconv.FormatFloat(projectID, 'f', 0, 64)+"&role=director&token="+adminToken, nil)
	if err != nil {
		return fmt.Errorf("WS连接失败: %v", err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("发送失败: %v", err)
	}
	return nil
}

func testLogsQuery() error {
	code, body, err := get("/api/logs?project_id="+strconv.FormatFloat(projectID, 'f', 0, 64), adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d, 期望 200", code)
	}
	b, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("返回格式错误")
	}
	if _, ok := b["messages"]; !ok {
		return fmt.Errorf("返回中无 messages 字段")
	}
	return nil
}

func testExportJSON() error {
	// from/to 是必填的：导出接口此前对整个项目历史做无条件 Find，
	// 既没有时间边界也没有行数上限，一次误点就可能把几百 MB 灌进内存。
	body := fmt.Sprintf(`{"project_id":%s,"from":%q,"to":%q}`,
		strconv.FormatFloat(projectID, 'f', 0, 64),
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
	req, _ := http.NewRequest("POST", baseURL+"/api/logs/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("状态码 %d, body: %s", resp.StatusCode, string(b))
	}
	return nil
}

func testExportCSV() error {
	body := fmt.Sprintf(`{"project_id":%s,"from":%q,"to":%q}`,
		strconv.FormatFloat(projectID, 'f', 0, 64),
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
	req, _ := http.NewRequest("POST", baseURL+"/api/logs/export/csv", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("状态码 %d, body: %s", resp.StatusCode, string(b))
	}
	return nil
}

func testPluginList() error {
	code, body, err := get("/api/plugins", adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d", code)
	}
	plugins, ok := body.([]any)
	if !ok || len(plugins) == 0 {
		return fmt.Errorf("插件列表为空: %v", body)
	}
	return nil
}

func testCleanupLogs() error {
	code, body, err := postAuth("/api/logs/cleanup", map[string]any{"days": 30}, adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("清理失败，状态码 %d，响应 %v", code, body)
	}
	return nil
}

// testCleanupAudited 确认清理动作自己留下了审计记录。
//
// 「清理日志」会删掉证据本身。如果它自己不写一条审计，「谁把证据清了」
// 就永远查不出来——而这正是审计最该回答的问题之一。
func testCleanupAudited() error {
	code, body, err := get("/api/admin/audit-logs?action=logs.cleanup", adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d", code)
	}
	b, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("返回格式错误: %v", body)
	}
	if total, _ := b["total"].(float64); total < 1 {
		return fmt.Errorf("清理动作没有留下审计记录: %v", b)
	}
	return nil
}

// testShotCuts 查切台报表。
//
// shot_cuts 是 1.4.0 新增的表：切台此前只以一段不透明 JSON 存在
// messages.content 里，既不能按时间查也不能按机位查。
func testShotCuts() error {
	code, body, err := get("/api/projects/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/shot-cuts", adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d", code)
	}
	b, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("返回格式错误: %v", body)
	}
	if _, ok := b["summary"]; !ok {
		return fmt.Errorf("返回中无 summary 字段")
	}
	// 第 13 步刚做过一次切台，这里必须真的查到记录——否则说明写入链路是断的。
	if total, _ := b["total"].(float64); total < 1 {
		return fmt.Errorf("切台后 shot_cuts 里仍然没有记录（写入链路没生效）")
	}
	return nil
}

func testProjectStats() error {
	code, body, err := get("/api/projects/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/stats", adminToken)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d", code)
	}
	b, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("返回格式错误")
	}
	if _, ok := b["message_count"]; !ok {
		return fmt.Errorf("返回中无 message_count 字段")
	}
	if _, ok := b["shot_cut_count"]; !ok {
		return fmt.Errorf("返回中无 shot_cut_count 字段")
	}
	return nil
}

// --- WebSocket 辅助 ---

func connectWS(role string, projectID int, token string) error {
	url := wsURL + fmt.Sprintf("?project_id=%d&role=%s", projectID, role)
	if token != "" {
		url += "&token=" + token
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("WS连接失败: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("读取欢迎消息失败: %v", err)
	}
	return nil
}
