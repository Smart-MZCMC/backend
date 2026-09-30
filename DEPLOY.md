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

系统不预置账号。**用户表为空时，第一个注册的人自动成为管理员**，
不需要登录态，也不需要额外密钥：

```sh
curl -X POST http://127.0.0.1:3000/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123456","display_name":"系统管理员"}'
```

成功返回 `201`，其中 `"role":"admin"`：

```json
{"id":1,"username":"admin","display_name":"系统管理员","role":"admin"}
```

约束：密码至少 6 位；用户名不超过 64 字符，只能用字母、数字、`_`、`.`、`-`
和中文。`role` 不用传——引导模式下会被忽略。

::: danger 这件事必须在开放端口前做完
`POST /api/auth/register` 是公开路由。在用户表为空之前，
**任何能访问到 3000 端口的人**都能抢先注册一个管理员。
第一个账号建好后接口会自动收紧为「仅管理员可调用」，但那之前没有保护。

验证已经收紧：

```sh
curl -X POST http://127.0.0.1:3000/api/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"test","password":"test123456","role":"admin"}'
# 期望：401 {"error":"系统已有账号，创建用户需要管理员登录"}
```
:::

## 五、启动

```sh
chmod +x smart-mzcmc start.sh
./start.sh
```

看到 `[WS] 服务器启动: :3002` 与路由表输出即启动成功。
**验证：** 浏览器打开 `http://<服务器IP>:3000/` ，首页右上角应显示「全部在线」。

## 六、注册 systemd（推荐）

```sh
sudo cp smart-mzcmc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now smart-mzcmc
sudo systemctl status smart-mzcmc
```

单元文件里 `WorkingDirectory` 指向解压目录，**这一行不能改**——
首页、`/admin`、`/docs` 三个静态站点是按进程工作目录解析的
（`./public/xxx`），跑错目录会 404。采访端则按可执行文件所在目录解析，两者规则不同。

看日志：

```sh
sudo journalctl -u smart-mzcmc -f
tail -f storage/logs/goravel.log
```

## 端口

| 端口 | 用途 | 是否可对外 |
| --- | --- | --- |
| 3000 | HTTP API、首页、管理后台、文档站 | 见下 |
| 3002 | WebSocket Hub、采访端 Web | 见下 |

**推荐做法：两个端口都只监听 `127.0.0.1`，对外只暴露反向代理的 80/443。**
只开 3000 的话，首页会显示「WebSocket 不可达」，各端客户端会持续重连。

systemd 单元里 `ExecStart` 用的是本机地址，天然只监听回环——这一点不用额外配置。

### 反向代理（生产）

在前面放一层 nginx，把 3000 和 3002 收拢到同一个域名。
完整的配置说明和排错清单见 `docs/operation-manual.md` 的「反向代理部署」。

最小可用版本：

```nginx
# 必须在 http {} 块里。宝塔的 vhost 文件在 http 块内 include，所以写在 server 之前即可。
# 变量名加 smartmzcmc_ 前缀，避免和宝塔可能已定义的 $connection_upgrade 撞名。
map $http_upgrade $smartmzcmc_connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 80;
    server_name zhdb.647382.xyz;

    # 3002：WebSocket。用 ^~ 前缀匹配而非 = /ws，因为 /ws/status 也要走这里。
    # 不能写成 /ws/ —— 后端 Go ServeMux 把 "/ws" 注册为精确匹配。
    location ^~ /ws {
        proxy_pass http://127.0.0.1:3002;
        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $smartmzcmc_connection_upgrade;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        # 默认 60s 会掐掉空闲连接；笔记本休眠时客户端心跳会停，
        # 60s 断线会让服务端释放导播控制权，画面可能被另一位导播抢走。
        proxy_read_timeout 300s;
        proxy_buffering off;
    }

    # 3002：采访端
    location ^~ /interviewer/ {
        proxy_pass http://127.0.0.1:3002;
        proxy_set_header Host $host;
    }

    # 3000：其余全部
    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

::: warning 用宝塔面板时务必检查两点
1. **删掉 `include /www/server/panel/vhost/rewrite/go_*.conf;`**
   那是宝塔「Go 项目」框架的伪静态模板，会重写查询串。本项目所有接口都靠
   查询串传参（`/ws?project_id=1&role=director&token=...`），被重写后各端直接连不上。
2. **不要把 `Connection` 写死成 `"upgrade"`**
   那样普通 HTTP GET 也会带上 `Connection: upgrade`，破坏上游 keepalive。
   用上面的 `map`。
:::

验证：

```sh
curl -I http://zhdb.647382.xyz/                         # 200
curl    http://zhdb.647382.xyz/ws/status                # {"status":"ok",...}
curl -I http://zhdb.647382.xyz/interviewer/             # 200
curl -I http://zhdb.647382.xyz/interviewer/config.json  # 200
```

`curl` 测不出 WebSocket（不发 Upgrade 头），直接看系统首页右上角的连接状态最快。

### 配好反代后，各端填什么

| 端 | 配置文件 | 值 | 要重新编译？ |
| --- | --- | --- | --- |
| 管理后台 | 无需配置 | — | 否 |
| 解说端 | exe 同目录 `config.json` | `"WsUrl": "ws://zhdb.647382.xyz/ws"` | **否**，重启 exe |
| 包装端 | exe 同目录 `config.json` | 同上 | **否**，重启 exe |
| 采访端 | `public/interviewer/config.json` | `"wsUrl": "ws://zhdb.647382.xyz/ws"` | **否**，刷新浏览器 |
| 导播端 | `director/lib/config.dart` | `serverUrl` + `wsUrl` | **是** |

::: danger 启用 HTTPS 之后，全部改成 `wss://`
浏览器把 https 页面里的 `ws://` 判为**混合内容**直接拦掉，连不上而且**不报错**——
表现就是各端状态灯一直转圈。需要改：两个桌面端的 `config.json`、
`public/interviewer/config.json`、以及 `director/lib/config.dart`（并重新编译）。
`serverUrl` 同理改成 `https://`。
:::

## 防火墙

配了反向代理时，只放通代理的 80/443：

```sh
sudo firewall-cmd --add-port=80/tcp --permanent
sudo firewall-cmd --add-port=443/tcp --permanent
sudo firewall-cmd --reload
```

没配反代、要直连 3002 的话才需要额外放通 3000 和 3002：

```sh
sudo firewall-cmd --add-port=3000/tcp --permanent
sudo firewall-cmd --add-port=3002/tcp --permanent
sudo firewall-cmd --reload
```

## 常见问题

**启动报 `Please initialize APP_KEY first`**
`.env` 里的 `APP_KEY` 是空的，或 `.env` 不在当前工作目录。`APP_KEY` 必须是 32 位字符串。

**访问 `/admin` 或 `/docs` 返回 404，页面显示「站点产物未就绪」**
进程工作目录不对。`WorkingDirectory` 必须指向包含 `public/` 的那一层。
用 `./start.sh` 启动或保持 systemd 单元文件里的 `WorkingDirectory` 即可。

**首页显示「WebSocket 不可达」**
3002 没起来，或反代没配 `/ws`。先在服务器本机验证：

```sh
curl http://127.0.0.1:3002/ws/status     # 期望 {"status":"ok",...}
```

本机通、但首页说不可达 → 反代少了 `location ^~ /ws`，或者写成了 `/ws/`
（后端 `ServeMux` 把 `/ws` 注册为精确匹配，带斜杠匹配不上）。

**日志报 `sqlite3: unable to open database file`**
工作目录不可写。程序会自动创建 `database/`，但前提是当前目录有写权限。

**各端客户端连不上**
按部署形态二选一，别混用：

| 形态 | `WsUrl` |
| --- | --- |
| 直连 | `ws://<服务器IP>:3002/ws` |
| 反代 | `ws://<域名>/ws` |

别填 `127.0.0.1`——那是客户端自己。

**采访端页面白屏（所有资源 404）**
`flutter build web` 漏了 `--base-href /interviewer/`。用
`interviewer/build-web.bat` 构建，它已经把参数带上了。详见 `interviewer/README.md`。

**采访端能打开但一直转圈连不上**
`public/interviewer/config.json` 里的 `wsUrl` 写成了 `ws://` 而页面是 `https://`
——混合内容被浏览器拦掉，且**不报错**。改成 `wss://`。

**管理后台「新建用户」报 401**
说明当前没有管理员登录态。该接口在用户表非空后只接受管理员调用，
这是预期行为。用第一个管理员账号登录后再操作。

**有人抢先注册了管理员**
如果端口在创建首个账号前就对外开放过，库里可能已有陌生账号。
用管理员账号登录后进「用户管理」删掉它；或者直接停服、
清空 `database/smart-mzcmc.db` 后重启（会丢失全部数据），
再重新走一遍初始化流程。

**改了首页但浏览器还是旧内容**
已带 `Cache-Control: no-cache`，正常会自动回源校验。
若前面挂了 Nginx 且它自己配了缓存，需要在 Nginx 侧也对 HTML 关缓存。

## 升级

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
