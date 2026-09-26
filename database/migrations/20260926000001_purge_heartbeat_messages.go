package migrations

import (
	"log"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

// M20260926000001PurgeHeartbeatMessages 清理历史心跳消息。
//
// 背景：hub 之前在写 messages 表之后才过滤 heartbeat，导致每个客户端每 10 秒
// 一条心跳全部落库。1401 条历史记录里有 1251 条是心跳，日志审计页和
// message_count 基本被噪音淹没。
//
// 现在心跳在入库前就被丢弃（见 app/ws/hub.go 的 IsHeartbeat），
// 本迁移负责把已经写进去的历史噪音清掉。
//
// 识别条件与 IsHeartbeat 保持一致：type = 'chat' 且 content 恰为
// {"message":"heartbeat"}。content 是 TEXT 存的原始 JSON 字符串，
// 这里用等值比较而非模糊匹配，避免误删正文里恰好提到 heartbeat 的消息。
type M20260926000001PurgeHeartbeatMessages struct{}

func (m *M20260926000001PurgeHeartbeatMessages) Signature() string {
	return "20260926000001_purge_heartbeat_messages"
}

func (m *M20260926000001PurgeHeartbeatMessages) Up() error {
	if !facades.Schema().HasTable("messages") {
		return nil
	}

	result, err := facades.Orm().Query().
		Where("type = ?", "chat").
		Where("content = ?", `{"message":"heartbeat"}`).
		Delete(&models.Message{})
	if err != nil {
		return err
	}
	if result.RowsAffected > 0 {
		log.Printf("[Migration] 已清理 %d 条历史心跳消息", result.RowsAffected)
	}
	return nil
}

func (m *M20260926000001PurgeHeartbeatMessages) Down() error {
	// 删掉的数据无法恢复，Down 只能什么都不做。
	// 心跳本来就是噪音，没有保留价值。
	return nil
}
