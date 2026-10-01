# 部署说明（Linux amd64）

本目录是一个可直接运行的发布包。解压后先改 `.env`，再执行迁移，然后启动。

> **这些包从哪来？** GitHub Releases。给本仓库打 tag 即自动构建发布：
>
> ```sh
> git tag v1.2.0
> git push origin v1.2.0
> ```
>
> 工作流会构建后端发布包与两个客户端 APK，产出草稿 Release（**默认不直接发布**，
> 需要人工确认后在 Releases 页面点 Publish）。详见本文末「发布流程」。
>
> 离线构建：`cd backend && go run ./tools` → `dist/backend-linux-amd64.tar.gz`。
> 两者用的是同一个打包器，产物一致。

## 目录结构

```
.
├── smart-mzcmc              # 主程序（Linux x86-64，静态链接，无需 glibc 依赖）
├── .env                     # 配置（从 .env.example 复制后修改）
├── .env.example             # 配置模板
├── public/
│   ├── index.html           # 系统首页
│   ├── admin/               # 管理后台（SvelteKit 静态产物）
│   ├── docs/                # 文档站（VitePress 静态产物）
│   └── interviewer/         # 采访端 Web（Flutter 静态产物）
├── resources/views/         # 模板目录（Goravel 视图）
├── database/                # SQLite 数据文件存放目录（首次启动自动创建）
├── storage/
│   ├── logs/                # 运行日志
│   └── framework/sessions/  # 会话文件
├── start.sh                 # 启动脚本
├── smart-mzcmc.service      # systemd 单元文件
└── DEPLOY.md                # 本文件
```

## 一、准备

```sh
tar -xzf backend-linux-amd64.tar.gz
cd backend-linux-amd64      # 目录名以实际压缩包内的顶层目录为准
```

要求：Linux x86-64、glibc 2.17 及以上、内核 3.2 及以上。
二进制是静态链接，不需要服务器安装 Go 或任何运行库。

## 二、配置

```sh
cp .env.example .env
vi .env
```

**必须修改的两项：**

| 配置项 | 说明 |
| --- | --- |
| `JWT_SECRET` | 留空会导致无法签发令牌、登录不了。换成随机字符串，例如 `openssl rand -hex 32` |
| `APP_KEY` | 32 位字符串。留空后端会直接拒绝启动 |

**上线前建议一并调整：**

| 配置项 | 建议值 |
| --- | --- |
| `APP_ENV` | `production` |
| `APP_DEBUG` | `false` |
| `APP_HOST` | `0.0.0.0`（需要局域网访问时） |

## 三、初始化数据库

首次部署必须执行一次，建表并清掉历史心跳消息：

```sh
./smart-mzcmc migrate
```

该命令幂等，重复执行安全。升级版本后如果新增了迁移，再跑一次即可。

## 四、创建管理员账号

系统不预置账号。**用户表为空时，第一个注册的人自动成为超级管理员**，
不需要登录态，也不需要额外密钥：

```sh
curl -X POST http://127.0.0.1:3000/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123456","display_name":"系统管理员"}'
```

成功返回 `201`，其中 `"role":"super_admin"`：

```json
{"id":1,"username":"admin","display_name":"系统管理员","role":"super_admin"}
```

约束：密码至少 6 位；用户名不超过 64 字符，只能用字母、数字、`_`、`.`、`-`
和中文。`role` 不用传——引导模式下会被忽略。

**为什么是超级管理员而不是管理员**：只有超级管理员能授予超级管理员角色。
引导出来的若是管理员，就再没有人能创建超管——系统会停在「谁也管不了谁」的
状态，系统更新与角色调整全都做不了。

::: danger 这件事必须在开放端口前做完
`POST /api/auth/register` 是公开路由。在用户表为空之前，
**任何能访问到 3000 端口的人**都能抢先注册一个管理员。
第一个账号建好后接口会自动收紧为「仅管理员可调用」，但那之前没有保护。
:::

验证已经收紧：

```sh
curl -X POST http://127.0.0.1:3000/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"test","password":"test123456","role":"admin"}'
# 期望：401 {"error":"系统已有账号，创建用户需要管理员登录"}
```

### 存量部署升级到超级管理员

老版本引导出来的是 `admin`。升级时会自动把**最早创建的那个管理员**提升为
超级管理员（见 `database/migrations/20261001000001_ensure_super_admin.go`，
已通过 `./smart-mzcmc migrate` 自动执行，日志里会明确告知提了谁）。

提完请尽快在管理后台指定第二个超级管理员，别让系统只有一个人管得了。

### 角色与权限

角色按等级排列，高角色自动拥有低角色的全部接口权限（后端 `RequireRole`
收的是「最低要求」，不是等值比较）：

| 角色 | 等级 | 能做什么 |
| --- | --- | --- |
| `super_admin` 超级管理员 | 60 | 全部。独占系统信息与在线更新 |
| `admin` 管理员 | 50 | 用户、项目、权限分配、日志清理 |
| `leader` 负责人 | 40 | 额外可导出数据 |
| `pre_production` 前期 | 30 | 登录、看板与日志读取 |
| `logistics` 后勤 | 20 | 同上 |
| `director` 导播 | 10 | 同上，外加操作被分配项目的切台 |

管账号（改角色、删人）**一律要求管理员及以上，与等级无关**——否则负责人能
自己升成管理员，后勤能删掉导播账号。

后端另有四道拦截（`app/http/controllers/authz.go`）：

- 不能授予高于自己的角色
- 不能修改权限不低于自己的账号
- 不能降低自己的权限（避免把自己锁在门外）
- 不能删除自己的账号

对 403 的说明：客户端把它当作「当前用户的真实权限状态」，**不会**踢掉登录态，
所以无权的操作会停在当前页面并提示原因。

## 用户中心

管理后台「个人中心」在**头像菜单**里（不占侧边栏）：改显示名、邮箱、密码。

### 邮箱

仅用于匹配头像，**不参与登录、不会收到任何邮件**。全局唯一，可以留空。
写入前统一归一化为「去首尾空格 + 转小写」，所以 `A@x.com` 与 `a@x.com` 视为
同一个地址（否则两个人会共用一张头像，而且用户会收到莫名其妙的「已被占用」）。

数据库里 `users.email` 上建的是**部分索引**：

```sql
CREATE UNIQUE INDEX users_email_unique ON users(email)
  WHERE email IS NOT NULL AND email != ''
```

不是普通唯一索引。普通索引会让**第二个没填邮箱的账号建不出来**——
模型里 Email 是普通 string，GORM 给未填邮箱的账号插入的是空串 `''`，
唯一索引把两个 `''` 判成冲突，而注册接口当时报的是「用户名已存在」，
错误信息还完全指错了方向。

### 头像

来自 [WeAvatar](https://weavatar.com)，按 `md5(邮箱去空格转小写)` 取，
与 Gravatar 系服务一致。地址可在 `.env` 里改（`AVATAR_BASE_URL`），
头像 URL 由后端算好返回——浏览器的 Web Crypto 只有 SHA 系列、没有 MD5，
放前端算得引第三方依赖。

> **本系统不能上传头像。** 需要真头像的用户要先在 weavatar.com 注册、绑定
> 上面这个邮箱并完成验证；没注册过的邮箱会拿到一个字母头像。
>
> 内网无外网时头像可能加载不出来，此时自动回退为首字母圆圈，不影响使用。
> 头像请求带 `referrerpolicy="no-referrer"`，不会把内网 Origin 带给外部站。

### 改密码

`PUT /api/auth/password`，需要当前密码 + 新密码。改完：

- 库里的 `token_version` 递增，令牌里带的是签发时的版本号，**所有旧令牌立即失效**
- 三处验签点都比对：HTTP 中间件、`resolveActor`（公开路由自解析）、WebSocket 鉴权
- 同时返回**新令牌**，当前设备自动续期，不会被自己踢下线
- 其他设备上的导播端 WebSocket 连接会一并断开

> 升级前签发的令牌仍可用（令牌里没有版本号，按 0 处理，与数据库默认值一致），
> 到期后自然失效。若要求升级即失效，需要让用户重新登录一次。

## 健康检查

```
GET /api/health      公开
```

给监控与反向代理探活用。它会真的发一条数据库查询——只 ping 连接是不够的：
SQLite 连接池在文件被挪走或磁盘出问题时可能还握着 fd，要 SELECT 才会暴露。

数据库不可用时返回 **503**（不是 200），这样负载均衡与监控才能真的把实例摘下去：

```json
{"status":"unhealthy","version":"1.1.0","uptime_seconds":3721,
 "checks":{"database":false},"detail":"..."}
```

`detail` 只在调用者是超级管理员时返回数据库错误详情——探针会把整个响应体记进
日志，无条件带上驱动名和文件路径等于把内部信息散到各处。

`GET /api/status` 是给前端用的旧接口（只报在线人数与版本，不做任何检查），
保留是为了兼容，不要拿它当探针。

::: tip 配 Nginx 探活
```nginx
location = /api/health {
    proxy_pass http://127.0.0.1:3000;
    access_log off;
}
```
:::

## 升级

手工升级：

```sh
sudo systemctl stop smart-mzcmc
sudo cp /path/to/new/smart-mzcmc ./smart-mzcmc
sudo cp -r /path/to/new/public .          # 静态站点整体替换
./smart-mzcmc migrate                      # 若版本新增了迁移
sudo systemctl start smart-mzcmc
```

`database/` 与 `storage/` 不要覆盖，它们是运行期数据。

> 换服务器地址**不需要**重新部署后端——解说端、包装端、采访端的地址都在各自
> 的配置文件里，运行期改。只有导播端要重新 `flutter build`。

### 在线更新

管理后台「系统设置」页（仅超级管理员可见）可以检查并应用后端更新，
省掉 SSH 上去换文件。默认关闭，要显式打开：

```sh
UPDATE_ENABLED=true
UPDATE_ALLOW_REPLACE=false     # 先用「只下载校验」模式试一次
```

流程：

1. 查 GitHub Release 的最新版本，与本地 `Version` 比较
2. 下载 `backend-linux-amd64.tar.gz`
3. 拉取同一 Release 的 `checksums.txt`，比对 sha256
4. 解压出 `smart-mzcmc`，确认是 ELF 且架构正确
5. 备份当前程序为 `smart-mzcmc.bak`
6. 同目录临时文件 + `rename` 原子替换（跨文件系统 rename 会失败）
7. **用新二进制跑一次 `migrate`**；失败就把备份换回去
8. 进程退出，由 systemd 拉起新版本

安全上的取舍，逐条都是踩过的坑：

- **校验和不匹配一律拒绝，查不到条目同样视为不通过** —— 后者最容易写错成
  「查不到就放行」，那样一个被裁剪过的 `checksums.txt` 就能让校验形同虚设。
- **解压防路径穿越与压缩炸弹**。归档里的文件名是不可信输入，直接 Join 到目标
  目录，一个 `../../etc/cron.d/x` 就能写到目标之外。同时拒绝符号链接与设备节点。
- **必须先确认 `migrate` 通过再退出**。否则 systemd 拉起一个起不来的版本，
  故障就从「更新失败」变成「服务不可用」。
- **必须显式传目标版本号**（界面上你确认的那个），后端会与更新源当前版本比对，
  消除「检查时是 1.2.0、下载时装上 1.3.0」的竞态。

::: danger `UPDATE_ALLOW_REPLACE=true` 的前提
替换后本进程会退出。**只有托管在 systemd 之下才会被重新拉起**；若只是直接
`./smart-mzcmc` 起的，退出就等于停服，会变成一次线上事故。

systemd 单元里确认有 `Restart=always`（单元文件里已经带了）。
:::

内网无外网时，把 `UPDATE_SERVER` 指向一个 GitHub Releases 兼容的镜像或代理地址。

回滚：`smart-mzcmc.bak` 就在程序同级目录，出问题直接换回来再 `systemctl start`。

---

## 发布流程

打 tag 即触发 `.github/workflows/release.yml`：

```sh
git tag v1.2.0
git push origin v1.2.0
```

产物：

| 文件 | 说明 |
| --- | --- |
| `backend-linux-amd64.tar.gz` | 本文档所在的发布包 |
| `director-<版本>.apk` | 导播端 |
| `interviewer-<版本>.apk` | 采访端 |
| `checksums.txt` | 上述三个文件的 sha256 |

### 客户端仓库的 ref 怎么定

工作流优先按**同名 tag** 拉取 admin / docs / director / interviewer；
某个仓库没打这个 tag 时回退 `main`，并把实际用的 ref 写进 Release 说明。
发布说明必须反映真实来源，不能装作可复现。

想让发布完全可复现，就在五个仓库打同名 tag：

```sh
for r in admin docs director interviewer; do
  git -C ../$r tag v1.2.0 && git -C ../$r push origin v1.2.0
done
```

### Release 是草稿

工作流用 `draft: true`，**不会自动发布**。请到 Releases 页面检查后手动点
Publish。理由是 APK 和域名相关配置出错时后果比较直接，值得人工过一眼。

### APK 签名

配了这四个仓库 secret 就用正式签名：

| Secret | 说明 |
| --- | --- |
| `ANDROID_KEYSTORE_BASE64` | `base64 -w0 upload-keystore.jks` 的输出 |
| `ANDROID_KEYSTORE_PASSWORD` | keystore 密码 |
| `ANDROID_KEY_ALIAS` | 密钥别名 |
| `ANDROID_KEY_PASSWORD` | 密钥密码 |

没配则退回 **debug 签名**，工作流会打 `::warning::` 提示。这种包无法覆盖安装、
无法上架，只适合内网测试。

生成 keystore（`PKCS12` 是现在的标准格式，keytool 会对 JKS 给出迁移提示）：

```sh
keytool -genkeypair -keystore upload-keystore.jks -storetype PKCS12 \
        -keyalg RSA -keysize 2048 -validity 10000 -alias upload
base64 -w0 upload-keystore.jks
```

> PKCS12 要求 store 与 key 用同一个密码，`-keypass` 传不同值会被 keytool 忽略并
> 告警。工作流的 `ANDROID_KEYSTORE_PASSWORD` 与 `ANDROID_KEY_PASSWORD` 填同一个值即可。

正式签名时工作流还会解包 APK 检查签名块是否真的存在，签名没生效会直接失败
——否则正式包和 debug 包长得一模一样，发出去才发现。

### 应用包名与签名已定稿

包名已从模板残留值改为自有反向域名，**1.1.0 起不可再改**：

| 端 | `applicationId` |
| --- | --- |
| 导播端 | `top.laobinghu.smart.mzcmc.director` |
| 采访端 | `top.laobinghu.smart.mzcmc.interviewer` |

注意三处必须一致：gradle 里的 `namespace`、`applicationId`，以及
`MainActivity.kt` 的 `package` 声明。AndroidManifest 里 activity 写的是
`android:name=".MainActivity"`，这个简写按 `namespace` 解析 —— 只改
`namespace` 而忘了 `MainActivity`，会直接编译失败（解析出来的类不存在）。

> `namespace` 与 `applicationId` 是两回事：前者决定 R 类 / BuildConfig 的生成
> 包名，后者才是装到设备上的包标识。只改 `namespace` 对安装包名毫无作用。

签名：`alias=upload`，RSA 2048，PKCS12，有效期至 2054 年，两个客户端共用一把。
证书 SHA-256：

```
D0:41:83:95:E4:E8:F7:30:C8:53:9F:62:5C:8F:23:76:EA:C9:38:93:67:6C:F2:DF:BE:20:9A:BC:58:06:A5:66
```

> **keystore 必须长期备份。** 丢失后无法再为已发布的应用签出可覆盖升级的包，
> 只能让所有人卸载重装（本地数据会丢）。它不在版本库里，只存在于后端仓库的
> `ANDROID_KEYSTORE_*` secrets 与你自己的备份中。

本地构建要用同一把密钥签，需要在各客户端目录下放 `android/key.properties`
（已被 `android/.gitignore` 忽略），字段见 `backend/android/key.properties.tmpl`。
不放则退回 debug 签名——那种包无法覆盖安装，也无法上架。
