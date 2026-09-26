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
go run .                        # 开发运行
go run . migrate                # 只跑数据库迁移，不启动服务
go build -o smart-mzcmc .       # 编译
```

> Goravel 的 `migrate` 原本是 console 命令，但本项目没有接入 console kernel，
> 所以 `main.go` 里直接遍历 `bootstrap.Migrations()` 调 `Up()`，并且**没有记账表**——
> 所有迁移都必须写成幂等的（建表判 `HasTable`、清理用 `DELETE`）。
> 新增迁移后记得在 `docs/development-guide.md` 的迁移表里补一行。

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
├── routes/
│   ├── web.go                # HTTP 路由
│   └── staticSite.go         # 静态站点托管中间件
└── tools/package.go          # 交叉编译并打 Linux 发布包
```

## 打包发布

```sh
# 1. 先把三个静态站点构建到 public/
cd ../admin        && pnpm run build:deploy          # -> public/admin
cd ../docs         && pnpm run docs:build:deploy    # -> public/docs
cd ../interviewer  && flutter build web              # -> public/interviewer

# 2. 交叉编译 + 打包
cd ../backend
go run ./tools
# -> ../dist/backend-linux-amd64.tar.gz
```

`tools/package.go` 会：

- 以 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 交叉编译。SQLite 走
  `ncruces/go-sqlite3` 纯 Go 实现，**不需要任何交叉编译工具链**；
  打完会校验产物确实是 ELF + x86-64，不是就直接报错退出
  （Windows 上如果 `GOOS` 写错，会静默产出 `.exe` 而不是 Linux 程序）。
- 组装「二进制 + `public/` + `resources/` + `.env.example` + `start.sh` +
  systemd 单元 + `DEPLOY.md`」，并给运行期目录放 `.keep` 占位。
- 打成带顶层目录的 `tar.gz`，二进制与 `start.sh` 置 `0755`，
  `start.sh` 的 CRLF 转 LF，时间戳归零（便于逐字节比对两次构建）。

任一站点产物缺失会直接失败并提示该去哪个仓库构建。

::: warning 三个站点缺一不可
`public/admin`、`public/docs`、`public/interviewer` 都要在。
缺任何一个，启动日志会明确告诉你缺哪个、期望路径是什么，
对应入口返回带说明的 404，而不是一个空白页。
:::

`Dockerfile` 与 `.github/workflows/ci.yml` 里有同样的交叉编译步骤，
CI 会把跨仓库的 admin / docs / interviewer 源码一并拉下来构建。

## 运行期目录

以下目录都相对**进程工作目录**，程序启动时会自动创建：

| 目录 | 用途 |
| :--- | :--- |
| `database/` | SQLite 数据文件（`DB_DATABASE` 默认 `database/smart-mzcmc.db`） |
| `storage/logs/` | 日志文件 |
| `storage/framework/sessions/` | file session 驱动的落盘位置 |

::: danger 工作目录必须是包含 `public/` 的那一层
`./`（首页）、`/admin`、`/docs` 三个静态站点按**进程工作目录**解析
（`./public/xxx`），而采访端按**可执行文件所在目录**解析——两者规则不同。
在错误目录启动时，采访端仍然正常，但首页 404、`/admin` 与 `/docs`
返回「站点产物未就绪」。用 `./start.sh` 启动或保留 systemd 单元里的
`WorkingDirectory` 即可。
:::

## API

### 公开

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| `POST` | `/api/auth/login` | 登录，返回 JWT 与用户信息 |
| `POST` | `/api/auth/register` | 创建用户，**双模式**，见下方说明 |
| `GET` | `/api/status` | 服务状态与在线连接数 |
| `GET` | `/api/interview/:projectId` | 查询项目下采访点状态 |
| `POST` | `/api/interview/status` | 采访端上报状态 |

#### `POST /api/auth/register` 的双模式

系统不预置账号。**用户表为空时，第一个注册的人自动成为管理员**，
不需要登录态，也不需要额外密钥：

```bash
curl -X POST http://localhost:3000/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123456","display_name":"系统管理员"}'
# => 201 {"id":1,...,"role":"admin"}
```

第一个账号建好后，同一接口立刻切换为「仅管理员可调用」——管理后台的
「新建用户」走的也是它。此时行为：

| 系统状态 | 需要认证 | `role` 字段 |
| :--- | :--- | :--- |
| 用户表为空 | 否 | 被忽略，强制 `admin` |
| 已有用户 | 需要 `admin` | 仅接受 `admin` / `director` |

参数约束：密码 ≥6 位；用户名 ≤64 字符，仅限字母、数字、`_`、`.`、`-`、中文。
状态码：401 未登录、403 角色不足、400 参数不合法、409 用户名已存在。

> ⚠️ 在用户表为空之前，任何能访问 3000 端口的人都能抢先注册管理员。
> 部署后请立刻创建第一个账号。

### 需要 JWT（`Authorization: Bearer <token>`）

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| `GET` | `/api/auth/profile` | 当前用户 |
| `POST` | `/api/locks/:projectId/acquire` | 抢占控制权 |
| `POST` | `/api/locks/:projectId/release` | 释放控制权 |
| `POST` | `/api/locks/:projectId/heartbeat` | 心跳续期 |
| `GET` | `/api/locks/:projectId/status` | 控制权状态 |
| `GET` | `/api/messages/:projectId` | 项目消息（最多 200 条） |
| `GET` | `/api/logs` | 日志查询，支持 `project_id` `type` `limit` |
| `GET` | `/api/plugins` | 已注册插件列表 |
| `GET` | `/api/projects/:projectId/stats` | 项目统计 |

### 需要 JWT + `admin` 角色

`Jwt` 中间件只验证令牌有效，不关心持有者是谁；这组接口额外挂了
`middleware.RequireRole("admin")`，每次请求查库比对角色（不信任令牌里
缓存的角色，因此后台降权会立即生效）。导播角色调用会得到 403。

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| `GET` | `/api/admin/users` | 用户列表 |
| `DELETE` | `/api/admin/users/:id` | 删除用户 |
| `PUT` | `/api/admin/users/:id/role` | 修改角色 |
| `GET` `POST` | `/api/admin/projects` | 项目列表 / 新建 |
| `PUT` `DELETE` | `/api/admin/projects/:id` | 编辑 / 删除项目 |
| `POST` | `/api/admin/assign` `/api/admin/revoke` | 授权 / 撤销授权 |
| `GET` | `/api/admin/users/:id/projects` | 某用户的授权列表 |
| `POST` | `/api/logs/export` `/api/logs/export/csv` | 导出日志（只读接口保持登录即可） |
| `POST` | `/api/logs/cleanup` | 清理超期日志 |

## WebSocket 协议

接入 `ws://<host>:3002/ws?project_id=1&role=director`。所有消息同构：

```json
{
  "type": "shot_state | chat | interview_status | lock_update | system",
  "project_id": 1,
  "sender_id": 2,
  "payload": { },
  "timestamp": 1789000000000
}
```

- `shot_state` 是唯一的切台消息：`payload = {"current": "正在播送", "next": "即将切台"}`，
  `next` 为空串表示已确认切完。转发给解说端与包装端，需要持有控制权。
  旧的 `next_shot` / `confirm_switch` 已被合并，服务端只回一条 `system` 错误并丢弃。
- 客户端通过 `chat` 类型、`payload.message = "heartbeat"` 维持心跳。
  心跳在入库前就被丢弃：不写 `messages` 表、不计入项目消息统计、不转发给其他端。
- `lock_update` 由服务端在抢占/释放/超时时广播
- `interview_status` 变更会同时广播给导播端与包装端并写库

## 插件配置

内置插件的配置集中在 `config/plugins.go`，全部走环境变量（见 `.env.example`）：

| 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `NTFY_SERVER` / `NTFY_TOPIC` | 空 | ntfy 告警地址与主题，两者都填才启用 |
| `PLUGIN_NTFY_ENABLED` | 空 | 显式开关；留空按配置是否齐全自动判断 |
| `PLUGIN_LOG_ARCHIVE_ENABLED` | `true` | 是否启用日志归档 |
| `PLUGIN_LOG_RETENTION_DAYS` | `30` | 日志保留天数 |
| `PLUGIN_LOG_CHECK_INTERVAL` | `1h` | 清理检查间隔，支持 `30s` / `5m` / `1h` |
| `PLUGIN_CSV_EXPORT_ENABLED` | `true` | 是否启用日志导出接口 |

`GET /api/plugins` 返回每个插件的 `name` / `version` / `description` / `enabled` / `reason` / `config`。
`config` 里的机密值（如 ntfy topic）已做脱敏。停用的插件依然会出现在列表里，
并带上 `reason` 说明停用原因——这样「配了但没生效」在界面上看得见，而不是只能翻日志。

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

**凭据配置：** 工作流用 `secrets.CROSS_REPO_TOKEN || github.token` 作为跨仓库 checkout 的凭据。

`Smart-MZCMC/admin` 与 `Smart-MZCMC/docs` 都是 public 仓库，默认的 `github.token` 通常就能读取，因此**不配也能跑**。但以下情况必须添加 secret `CROSS_REPO_TOKEN`：

- 把 `admin` 或 `docs` 改成 private
- checkout 步骤报 404

| Secret | 说明 |
| :--- | :--- |
| `CROSS_REPO_TOKEN` | 能读取 `Smart-MZCMC/admin` 与 `Smart-MZCMC/docs` 的 PAT |

- **Fine-grained token**：Repository access 选中 `admin`、`docs` 两个仓库，权限给 `Contents: Read-only`
- **Classic token**：勾选 `repo` scope

工作流里有一道显式校验，拉取失败时会直接提示需要配置这个 secret，而不是只抛一句 404。

如果希望 admin / docs 推送后主动触发后端重建，工作流里已经预留了 `repository_dispatch` 的 `frontend-updated` 事件类型；在 admin / docs 的 workflow 里加一个发事件的步骤即可。
