#!/bin/sh
# 启动脚本。
#
# 关键点：必须 cd 到脚本所在目录再启动。
# 首页、/admin、/docs 三个静态站点是按「进程工作目录」解析的（./public/xxx），
# 跑错目录会直接 404；而采访端是按可执行文件所在目录解析的，两者规则不同。
# 从别的目录调 ./start.sh 也能正常工作，就是靠这里的 cd。

set -e
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  echo "缺少 .env。请先执行: cp .env.example .env 并填写 JWT_SECRET 与 APP_KEY"
  exit 1
fi

# 预检配置，避免带着空密钥启动后才发现登录不了
if ! grep -qE '^[[:space:]]*APP_KEY=[[:space:]]*[0-9a-zA-Z]{32}[[:space:]]*$' .env; then
  echo "警告：.env 里的 APP_KEY 为空或不是 32 位字符串，后端会拒绝启动。"
  echo "     生成方法: openssl rand -hex 16"
fi

if ! grep -qE '^[[:space:]]*JWT_SECRET=.+' .env; then
  echo "警告：.env 里的 JWT_SECRET 为空，将无法登录。"
fi

# 首次部署或升级后建议执行一次 ./smart-mzcmc migrate（建表 + 数据清理，幂等）
if [ ! -f database/.initialized ]; then
  echo "首次启动，执行数据库初始化..."
  ./smart-mzcmc migrate
  touch database/.initialized
fi

exec ./smart-mzcmc "$@"
