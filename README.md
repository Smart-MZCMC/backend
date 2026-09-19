# smart-mzcmc-backend

绵中融媒体智汇导播系统的后端服务。基于 **Go 1.25 + [Goravel](https://www.goravel.dev) 1.18 + SQLite**，同时承担三件事：

1. **HTTP API**（`:3000`）—— 登录鉴权、管理后台接口、日志查询与导出
2. **管理后台与文档站托管**（`:3000`）—— 托管 `/admin`、`/docs` 两个静态站点
3. **WebSocket Hub**（`:3002`）—— 实时指令与状态推送，并托管采访端 Web

## 快速开始

> 后端同时托管管理后台和文档站，所以启动前需要先准备好这两个静态产物；否则 `/admin`、`/docs` 会是空白页面。

```bash
# 1. 准备管理后台静态产物 -> public/admin
cd ../admin && pnpm install && pnpm run build:deploy

# 2. 准备文档站静态产物 -> public/docs
cd ../docs && pnpm install && pnpm run build:deploy

# 3. 启动后端
cd ../backend
cp .env.example .env
```

`.env` 里至少需要设置：

```ini
APP_PORT=3000
JWT_SECRET=<换成你自己的密钥，留空会导致无法签发令牌>
DB_CONNECTION=sqlite
DB_DATABASE=database/smart-mzcmc.db
```

```bash
go run .          # 开发运行
go build -o smart-mzcmc .   # 编译
```

启动后：

| 地址 | 说明 |
| :--- | :--- |
| `http://127.0.0.1:3000` | 系统首页 |
| `http://127.0.0.1:3000/admin` | 管理后台 |
| `http://127.0.0.1:3000/docs` | 文档站 |
| `http://127.0.0.1:3002/interviewer/` | 采访端 Web |
| `ws://127.0.0.1:3002/ws` | WebSocket 接入点 |
| `http://127.0.0.1:3002/ws/status` | 在线连接数 |

## 目录结构

```
├── app/
│   ├── http/controllers/     # auth / admin / lock / message / interview / status
│   ├── http/middleware/jwt.go
│   ├── models/               # user / project / user_project / project_lock
│   │                         # / interview_status / message
│   ├── plugins/              # ntfy-alert / log-archive / csv-export
│   └── ws/                   # hub.go（消息路由）· server.go（:3002 服务）
├── bootstrap/app.go          # 应用装配：路由 + 全局中间件
├── config/                   # Goravel 配置
├── database/migrations/      # 6 张业务表 + jobs
├── public/                   # 静态资源（admin/ 与 docs/ 由构建脚本生成）
└── routes/
    ├── web.go                # HTTP 路由
    └── staticSite.go         # 静态站点托管中间件
```

## API

### 公开

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| `POST` | `/api/auth/login` | 登录，返回 JWT 与用户信息 |
| `POST` | `/api/auth/register` | 注册（管理后台新建用户也走这里） |
| `GET` | `/api/status` | 服务状态与在线连接数 |
| `GET` | `/api/interview/:projectId` | 查询项目下采访点状态 |
| `POST` | `/api/interview/status` | 采访端上报状态 |

### 需要 JWT（`Authorization: Bearer <token>`）

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| `GET` | `/api/auth/profile` | 当前用户 |
| `GET` `DELETE` | `/api/admin/users` `/api/admin/users/:id` | 用户列表 / 删除 |
| `PUT` | `/api/admin/users/:id/role` | 修改角色 |
| `GET` `POST` | `/api/admin/projects` | 项目列表 / 新建 |
| `PUT` `DELETE` | `/api/admin/projects/:id` | 编辑 / 删除项目 |
| `POST` | `/api/admin/assign` `/api/admin/revoke` | 授权 / 撤销授权 |
| `GET` | `/api/admin/users/:id/projects` | 某用户的授权列表 |
| `POST` | `/api/locks/:projectId/acquire` | 抢占控制权 |
| `POST` | `/api/locks/:projectId/release` | 释放控制权 |
| `POST` | `/api/locks/:projectId/heartbeat` | 心跳续期 |
| `GET` | `/api/locks/:projectId/status` | 控制权状态 |
| `GET` | `/api/messages/:projectId` | 项目消息（最多 200 条） |
| `GET` | `/api/logs` | 日志查询，支持 `project_id` `type` `limit` |
| `POST` | `/api/logs/export` | 导出项目日志 JSON |
| `POST` | `/api/logs/cleanup` | 清理超期日志 |
| `GET` | `/api/plugins` | 已注册插件列表 |
| `GET` | `/api/projects/:projectId/stats` | 项目统计 |

## WebSocket 协议

接入 `ws://<host>:3002/ws?project_id=1&role=director`。所有消息同构：

```json
{
  "type": "next_shot | confirm_switch | chat | interview_status | lock_update | system",
  "project_id": 1,
  "sender_id": 2,
  "payload": { },
  "timestamp": 1789000000000
}
```

- 客户端通过 `chat` 类型、`payload.message = "heartbeat"` 维持心跳
- `lock_update` 由服务端在抢占/释放/超时时广播
- `interview_status` 变更会同时广播给导播端与包装端并写库
- 状态型消息在重连后只推最新状态，不做全量补发

## 控制权锁

同一项目同一时间只允许一名导播持有控制权：

- 抢占写入 `project_locks`，带 `expire_at`
- 导播端周期性 `heartbeat` 续期，超时未续期则自动释放并广播通知
- 切换职责时由一方释放、另一方请求，状态实时同步

## 插件系统

`app/plugins/plugin.go` 提供 `Plugin` 接口与事件注册中心。主流程在关键节点（锁超时、导播掉线、采访离线、系统错误）发出事件，插件异步监听；插件 panic 会被 recover 捕获，不影响主系统。

| 插件 | 说明 |
| :--- | :--- |
| `ntfy-alert` | 通过 ntfy 推送到管理员手机。需配置 `server`/`topic`，未配置时自动禁用 |
| `log-archive` | 按保留天数（默认 30 天）定期清理过期消息日志 |
| `csv-export` | 日志导出 |

## 静态站点托管

`routes/staticSite.go` 以全局中间件方式托管两个静态站点：

| 前缀 | 目录 | 回退行为 |
| :--- | :--- | :--- |
| `/admin` | `public/admin` | 未命中 → `index.html`（SPA 路由） |
| `/docs` | `public/docs` | cleanUrls 补 `.html`；未命中 → `404.html` |

其中有几个 Goravel/gin 的坑，已在代码注释里记录：`{path...}` 只匹配单个路径段（无法覆盖多段资源路径）；`Route().Fallback()` 的返回值不会被渲染；`ctx.Response().File()` 在 Windows 上遇到绝对路径会静默返回空响应；`ctx.Request().Abort()` 会清空已写好的响应。

## 部署

`Dockerfile` 提供多阶段构建（`golang:alpine` 编译 → `alpine` 运行）。注意它只 `COPY` 仓库内的 `public/`，所以构建镜像前需要先把 admin 与 docs 的产物放进 `public/`。

### CI

`.github/workflows/ci.yml` 在每次推送时执行：跨仓库拉取 `Smart-MZCMC/admin` 与 `Smart-MZCMC/docs` 的源码 → 构建两个静态站点 → 复制到 `public/admin`、`public/docs` → `go vet` / `go test` / `go build` → 打包成 `backend-linux-amd64.tar.gz`。

**前置配置：** 需要在本仓库添加一个 secret：

| Secret | 说明 |
| :--- | :--- |
| `CROSS_REPO_TOKEN` | 能读取 `Smart-MZCMC/admin` 与 `Smart-MZCMC/docs` 的 PAT |

默认的 `GITHUB_TOKEN` 只能访问当前仓库，无法用于跨仓库 checkout，所以必须配 PAT。

- **Fine-grained token**：Repository access 选中 `admin`、`docs` 两个仓库，权限给 `Contents: Read-only`
- **Classic token**：勾选 `repo` scope

没配这个 secret 时，工作流会在 checkout `admin` 那一步失败。

如果希望 admin / docs 推送后主动触发后端重建，工作流里已经预留了 `repository_dispatch` 的 `frontend-updated` 事件类型；在 admin / docs 的 workflow 里加一个发事件的步骤即可。
