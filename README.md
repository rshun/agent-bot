# agent-bot

统一的 Telegram / Discord 私人 AI 助手网关，基于 Go 单二进制构建，内置 **Grok CLI** 与 **Codex CLI** 双引擎驱动（`BOT_ENGINE=grok|codex`）。

`agent-bot` 将原 `grok-bot` 与 `codex-bot` 的全部功能融合为同一套代码基线：公共能力（跨平台会话接力、长期记忆、多时段原生定时任务、本地受控脚本执行、MCP 管理、语音标准发音与多模态配图、SSRF 安全外链摘要、bubblewrap 沙箱隔离、零特权自升级）只维护一份；底层 CLI 差异通过 `internal/engine` 多态接口隔离。

---

## 核心特性

- **双引擎多态驱动 (`BOT_ENGINE=grok|codex`)**：同一编译产物既可驱动 Grok CLI（`grok`），也可驱动 OpenAI Codex CLI（`codex`），同时完整兼容旧环境变量（`GROK_*` / `CODEX_*`）。
- **Telegram + Discord 双平台同步**：支持跨平台历史会话一键接力（`/sessions`）、跨平台共享长期记忆（`/memory`）、引用回复上下文感知、流式工具调用进度条与交互按钮。
- **多模态与语音发音**：支持图片/文档上传理解、标准美音/英音语音合成条（`agentbot__pronounce`）、AI 生图与合规联网配图（`agentbot__send_web_image`）。
- **系统原生定时任务与脚本推送**：自动对接 Linux `crontab`、macOS `launchd`、Windows `Task Scheduler`，支持一日多时段（如 `每天 09:30, 15:00`）、单平台定向推送（`-p tg` / `-p dc`）及 `agent-bot send` 管道推送。
- **零特权安全自升级 (`/upgrade`)**：
  - 每个服务用户在各自目录拥有独立的二进制副本（如 `/home/grok/bin/agent-bot` 与 `/home/codex/bin/agent-bot`），全程无需 `sudo` 或 `root` 权限。
  - 默认 `/upgrade` 先展示版本与写权限检查面板，提供带版本锁的确认按钮（`/upgrade confirm <bot|cli|all> <tag>`）；同时支持 `/upgrade run [all|bot|cli]` 直跑与 `/upgrade auto [on|off]` 每日定时自动升级。
  - 内置 GitHub 官方域名 HTTPS 白名单、常量时间 SHA-256 校验、可执行文件魔数（ELF/PE/Mach-O）校验、`--version` 脱敏环境冒烟自检、SQLite 在线备份、`.prev` 自动回滚与 `syscall.Exec` 平滑进程切换（失败退回退出码 `42` 交由 `systemd` 拉起）。

---

## Debian 双用户部署指南（`grok` + `codex`）

推荐在 Debian / Ubuntu 服务器上采用 **`systemd` 模板单元 + 每用户独立二进制副本** 架构：

| 实例服务名 | Linux 运行用户 | 二进制路径（用户自有，支持零特权自升级） | 环境配置文件 | 数据目录 (`DATA_DIR`) |
| :--- | :--- | :--- | :--- | :--- |
| `agent-bot@grok` | `grok` | `/home/grok/bin/agent-bot` | `/etc/agent-bot/grok.env` | `/home/grok/.local/share/agent-bot` |
| `agent-bot@codex` | `codex` | `/home/codex/bin/agent-bot` | `/etc/agent-bot/codex.env` | `/home/codex/.local/share/agent-bot` |

### 1. 安装二进制到两个用户目录

可直接使用仓库提供的安装脚本自动下载最新 Release、校验 `SHA256SUMS` 并分别写入 `/home/grok/bin/agent-bot` 与 `/home/codex/bin/agent-bot`：

```bash
sudo bash deploy/install-user-bin.sh grok codex
```

或手动编译并分发到两个用户目录：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=v1.0.0" -o agent-bot ./cmd/agent-bot

sudo install -d -m 0755 -o grok -g grok /home/grok/bin
sudo install -m 0755 -o grok -g grok ./agent-bot /home/grok/bin/agent-bot

sudo install -d -m 0755 -o codex -g codex /home/codex/bin
sudo install -m 0755 -o codex -g codex ./agent-bot /home/codex/bin/agent-bot
```

> **提示**：为了让底层 CLI 也能在聊天内零特权升级，建议在 `codex` 用户下将 npm 全局前缀设为用户目录：
> ```bash
> sudo -iu codex mkdir -p /home/codex/.npm-global
> sudo -iu codex npm config set prefix '/home/codex/.npm-global'
> sudo -iu codex npm install -g @openai/codex@latest
> ```

### 2. 配置环境变量文件

```bash
sudo install -d -m 0755 /etc/agent-bot
sudo cp deploy/grok.env.example /etc/agent-bot/grok.env
sudo cp deploy/codex.env.example /etc/agent-bot/codex.env

# 仅允许对应服务用户读取各自配置文件
sudo chown root:grok /etc/agent-bot/grok.env && sudo chmod 0640 /etc/agent-bot/grok.env
sudo chown root:codex /etc/agent-bot/codex.env && sudo chmod 0640 /etc/agent-bot/codex.env
```

编辑 `/etc/agent-bot/grok.env` 与 `/etc/agent-bot/codex.env`，填入各自的 `TELEGRAM_BOT_TOKEN`、`DISCORD_BOT_TOKEN` 与 `ALLOWED_USER_IDS`（切勿将真实密钥提交到代码库）。

### 3. 注册并启动 `systemd` 模板服务

```bash
sudo cp deploy/agent-bot@.service /etc/systemd/system/agent-bot@.service
sudo systemctl daemon-reload

sudo systemctl enable --now agent-bot@grok
sudo systemctl enable --now agent-bot@codex

# 查看运行状态与日志
sudo systemctl status agent-bot@grok agent-bot@codex
sudo journalctl -u agent-bot@grok -f
sudo journalctl -u agent-bot@codex -f
```

---

## 从旧版 `grok-bot` / `codex-bot` 平滑迁移

`agent-bot` 的 SQLite 表结构（会话、长期记忆、定时任务、设置项）与旧项目完全兼容，启动时会自动完成增量列迁移并自动清理旧 `# BEGIN GROK-BOT TASKS` / `# BEGIN CODEX-BOT TASKS` crontab 块：

1. 停止旧服务前先备份原数据目录（例如 `cp -a /var/lib/grok-tg-bot /home/grok/.local/share/agent-bot` 并修正属主 `chown -R grok:grok /home/grok/.local/share/agent-bot`）。
2. 将原 `.env` 中的配置迁入 `/etc/agent-bot/grok.env`（加上 `BOT_ENGINE=grok`）与 `/etc/agent-bot/codex.env`（加上 `BOT_ENGINE=codex`）。
3. 停用旧单元并启用 `agent-bot@grok` / `agent-bot@codex`。
4. 原有外部脚本中使用的 `$GROK_BOT_BIN send` 或 `$CODEX_BOT_BIN send` 无需修改即可继续工作（`agent-bot` 会同时注入 `AGENT_BOT_BIN`、`GROK_BOT_BIN`、`CODEX_BOT_BIN`）。

---

## 常用聊天指令

| 指令 | 说明 |
| :--- | :--- |
| `/status` | 查看 `agent-bot` 与底层 CLI 版本、当前模型、会话、定时任务、自动升级状态与 MCP 概览 |
| `/upgrade` | 检查 `agent-bot` 与底层 CLI 更新及目录写权限，返回带版本锁确认按钮的升级面板 |
| `/upgrade run [all\|bot\|cli]` | 跳过面板直接执行一键升级（自动在线备份 SQLite、校验 SHA-256 并平滑重启） |
| `/upgrade auto [on\|off]` | 查看、开启或关闭每日定时自动升级（默认每天 `04:30` 空闲时执行） |
| `/model [名称]` | 查看或点选切换当前聊天模型 |
| `/new [标题]` / `/sessions` | 归档开启新话题 / 跨平台查看或接力切换历史会话 |
| `/memory` | 管理跨平台共享长期记忆（`/reset` 后仍保留） |
| `/task` | 自然语言或 Cron 创建、试跑、暂停、恢复、删除系统原生定时任务 |
| `/mcp` | 查看、诊断（`doctor`）、添加、启停、删除 MCP 服务 |
| `/usage` / `/quota` | 查看订阅剩余额度窗口与最近一轮 Token 消耗 |
| `/dir` / `/shell` | 管理沙箱可写目录白名单 / 开关服务器 Shell 执行权限 |
| `/stop` / `/reset` | 立即中断当前对话与排队队列 / 清空当前会话上下文 |

---

## 开发与验证

```bash
# 运行全部单元测试
go test -mod=readonly ./...

# 编译当前平台二进制
go build -mod=readonly -trimpath -o agent-bot ./cmd/agent-bot

# 查看版本与命令行推送帮助
./agent-bot --version
./agent-bot --help
```
