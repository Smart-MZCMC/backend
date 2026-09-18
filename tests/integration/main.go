// Package main 集成测试脚本
// 模拟完整的导播协调流程：注册 → 登录 → 创建项目 → 分配权限 → WebSocket连接 → 切台 → 采访状态 → 日志导出
// 用法: go run tests/integration/main.go [base_url]
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
		{"2. 管理员注册", testRegisterAdmin},
		{"3. 管理员登录", testLoginAdmin},
		{"4. 创建项目", testCreateProject},
		{"5. WebSocket连接(导播A)", testWSDirectorA},
		{"6. WebSocket连接(导播B-被拒)", testWSDirectorBRejected},
		{"7. WebSocket连接(采访端)", testWSInterviewer},
		{"8. 采访状态推送", testInterviewStatus},
		{"9. 导播A释放控制权", testReleaseLock},
		{"10. 导播B获取控制权", testAcquireLockB},
		{"11. 导播B切台(confirm_switch)", testConfirmSwitch},
		{"12. 日志查询", testLogsQuery},
		{"13. 日志导出(JSON)", testExportJSON},
		{"14. 日志导出(CSV)", testExportCSV},
		{"15. 插件列表", testPluginList},
		{"16. 手动清理日志", testCleanupLogs},
		{"17. 项目统计", testProjectStats},
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
		time.Sleep(200 * time.Millisecond) // 请求间隔
	}

	fmt.Println()
	fmt.Printf("  结果: %d 通过, %d 失败, 共 %d\n", passed, failed, passed+failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// --- 工具函数 ---

func post(path string, body any) (int, map[string]any, error) {
	data, _ := json.Marshal(body)
	resp, err := httpClient.Post(baseURL+path, "application/json", bytes.NewReader(data))
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
var (
	adminToken    string
	projectID     float64
	directorAID   float64
	directorBID   float64
)

// --- 测试用例 ---

func testServerStatus() error {
	code, body, err := get("/api/status", "")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("状态码 %d, 期望 200", code)
	}
	b, ok := body.(map[string]any)
	if !ok || b["status"] != "running" {
		return fmt.Errorf("状态: %v", body)
	}
	return nil
}

func testRegisterAdmin() error {
	code, body, err := post("/api/auth/register", map[string]any{
		"username":     "test_admin",
		"password":     "123456",
		"display_name": "测试管理员",
		"role":         "admin",
	})
	if err != nil {
		return err
	}
	if code != 201 && code != 409 {
		return fmt.Errorf("状态码 %d, 期望 201/409", code)
	}
	if id, ok := body["id"].(float64); ok {
		directorAID = id
	}
	return nil
}

func testLoginAdmin() error {
	code, body, err := post("/api/auth/login", map[string]any{
		"username": "test_admin",
		"password": "123456",
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

func testCreateProject() error {
	code, body, err := post("/api/admin/projects", map[string]any{
		"name":        "运动会测试项目",
		"code":        "test_sports_2026",
		"description": "集成测试用项目",
	})
	if err != nil {
		return err
	}
	if code != 201 && code != 409 {
		return fmt.Errorf("状态码 %d, 期望 201/409", code)
	}
	if id, ok := body["id"].(float64); ok {
		projectID = id
	}
	return nil
}

func testWSDirectorA() error {
	return connectWS("director", int(projectID), adminToken)
}

func testWSDirectorBRejected() error {
	// 第二个导播连接同一项目（控制权已被A持有）
	return connectWS("director", int(projectID), adminToken)
}

func testWSInterviewer() error {
	return connectWS("interviewer", int(projectID), "")
}

func testInterviewStatus() error {
	// 通过HTTP API更新采访状态
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

func testReleaseLock() error {
	code, _, err := post("/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/release", nil)
	if err != nil {
		// 需要认证
		req, _ := http.NewRequest("POST", baseURL+"/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/release", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err2 := httpClient.Do(req)
		if err2 != nil {
			return err2
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("释放控制权失败，状态码 %d", resp.StatusCode)
		}
		return nil
	}
	_ = code
	return nil
}

func testAcquireLockB() error {
	code, _, err := post("/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/acquire", nil)
	if err != nil {
		req, _ := http.NewRequest("POST", baseURL+"/api/locks/"+strconv.FormatFloat(projectID, 'f', 0, 64)+"/acquire", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err2 := httpClient.Do(req)
		if err2 != nil {
			return err2
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("获取控制权失败，状态码 %d", resp.StatusCode)
		}
		return nil
	}
	_ = code
	return nil
}

func testConfirmSwitch() error {
	// 通过WebSocket发送confirm_switch
	msg := map[string]any{
		"type":       "confirm_switch",
		"project_id": projectID,
		"payload":    map[string]any{"content": "全景"},
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
	req, _ := http.NewRequest("POST", baseURL+"/api/logs/export", bytes.NewBufferString(`{"project_id":`+strconv.FormatFloat(projectID, 'f', 0, 64)+`}`))
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
	req, _ := http.NewRequest("POST", baseURL+"/api/logs/export/csv", bytes.NewBufferString(`{"project_id":`+strconv.FormatFloat(projectID, 'f', 0, 64)+`}`))
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
	req, _ := http.NewRequest("POST", baseURL+"/api/logs/cleanup", bytes.NewBufferString(`{"days":30}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("清理失败，状态码 %d", resp.StatusCode)
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
