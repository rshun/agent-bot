# agent-bot

English | [简体中文](README_zh-CN.md)

A unified personal AI assistant gateway for **Telegram** and **Discord**, distributed as a single Go binary with polymorphic drivers for **Grok CLI (`grok`)**, **OpenAI Codex CLI (`codex`)**, and **Google Antigravity CLI (`agy`)** (`BOT_ENGINE=grok|codex|antigravity`).

`agent-bot` unifies the former `grok-bot` and `codex-bot` codebases into one shared core with native Google Antigravity CLI support: cross-platform session handoff, long-term memory, OS-native multi-slot scheduled tasks, guarded local script execution, MCP server management, US/UK pronunciation audio & multimodal images, SSRF-safe link prefetching, directory write allowlists, and zero-privilege self-upgrades are maintained once, while CLI differences are isolated behind polymorphic interfaces in `internal/engine`.

---

## Key Features

- **Tri-Engine Polymorphic Driver (`BOT_ENGINE=grok|codex|antigravity`)**: One compiled binary drives Grok CLI (`grok`), OpenAI Codex CLI (`codex`), or Google Antigravity CLI (`agy`), with backward-compatible support for legacy `GROK_*` / `CODEX_*` / `AGY_*` environment variables.
- **Telegram + Discord Unified Experience**: Cross-platform session handoff (`/sessions`), shared long-term memory (`/memory`), reply-quote context awareness, live streaming tool progress updates, and interactive inline buttons.
- **Multimodal & Voice Pronunciation**: Understands uploaded images and documents, generates US/UK pronunciation voice bars (`agentbot__pronounce`), supports AI image generation, and fetches SSRF-safe web images (`agentbot__send_web_image`).
- **OS-Native Scheduled Tasks & CLI Push**: Integrates with Linux `crontab`, macOS `launchd`, and Windows Task Scheduler, supporting multiple daily trigger times (e.g., `daily 09:30, 15:00`), targeted platform delivery (`-p tg` / `-p dc`), and `agent-bot send` piping.
- **Zero-Privilege Self-Upgrade (`/upgrade`)**:
  - Each service user runs its own writable copy of the binary (e.g., `/home/grok/bin/agent-bot`, `/home/codex/bin/agent-bot`, `/home/antigravity/bin/agent-bot`) with zero `sudo` or `root` privileges required at runtime.
  - Running `/upgrade` inspects bot and CLI versions plus directory write permissions and presents version-locked confirmation buttons (`/upgrade confirm <bot|cli|all> <tag>`), alongside `/upgrade run [all|bot|cli]` and `/upgrade auto [on|off]` for idle daily auto-upgrades.
  - Enforces official HTTPS host allowlists, constant-time SHA-256 / SHA-512 checksum verification, ELF/PE/Mach-O executable magic checks, sanitized `--version` smoke tests, live SQLite backups, `.prev` automatic rollback, and `syscall.Exec` in-place restart (falling back to exit code `42` for `systemd`).

---

## Debian Multi-User Deployment Guide (`grok` + `codex` + `antigravity`)

The recommended Debian/Ubuntu layout uses a **`systemd` template unit + per-user binary copies**:

| Service Unit | Linux User | Binary Path (User-Owned for Self-Upgrade) | Environment File | Data Directory (`DATA_DIR`) |
| :--- | :--- | :--- | :--- | :--- |
| `agent-bot@grok` | `grok` | `/home/grok/bin/agent-bot` | `/etc/agent-bot/grok.env` | `/home/grok/.local/share/agent-bot` |
| `agent-bot@codex` | `codex` | `/home/codex/bin/agent-bot` | `/etc/agent-bot/codex.env` | `/home/codex/.local/share/agent-bot` |
| `agent-bot@antigravity` | `antigravity` | `/home/antigravity/bin/agent-bot` | `/etc/agent-bot/antigravity.env` | `/home/antigravity/.local/share/agent-bot` |

### 1. Install Binaries into User Directories

Use the provided helper script to download the latest release, verify `SHA256SUMS`, and install into each user's `~/bin/agent-bot`:

```bash
sudo bash deploy/install-user-bin.sh grok codex antigravity
```

Or build from source and install manually:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=v1.0.0" -o agent-bot ./cmd/agent-bot

sudo install -d -m 0755 -o grok -g grok /home/grok/bin
sudo install -m 0755 -o grok -g grok ./agent-bot /home/grok/bin/agent-bot

sudo install -d -m 0755 -o codex -g codex /home/codex/bin
sudo install -m 0755 -o codex -g codex ./agent-bot /home/codex/bin/agent-bot

sudo install -d -m 0755 -o antigravity -g antigravity /home/antigravity/bin
sudo install -m 0755 -o antigravity -g antigravity ./agent-bot /home/antigravity/bin/agent-bot
```

> **Note (Codex)**: To allow privilege-free in-chat CLI upgrades, configure a user-local npm global prefix for `codex`:
> ```bash
> sudo -iu codex mkdir -p /home/codex/.npm-global
> sudo -iu codex npm config set prefix '/home/codex/.npm-global'
> sudo -iu codex npm install -g @openai/codex@latest
> ```
>
> **Note (Antigravity `agy`)**: Install the official CLI to `~/.local/bin/agy` under the `antigravity` user and complete one interactive sign-in (or set `GEMINI_API_KEY` with `"modelProvider": "gemini"` in `~/.gemini/antigravity-cli/settings.json`). `agent-bot` automatically configures `.agents/mcp_config.json` and `.agents/hooks.json` in the workspace to enforce `/dir` and `/shell` security policies:
> ```bash
> sudo -iu antigravity bash -c 'curl -fsSL https://antigravity.google/cli/install.sh | bash'
> sudo -iu antigravity ~/.local/bin/agy
> ```

### 2. Configure Environment Files

```bash
sudo install -d -m 0755 /etc/agent-bot
sudo cp deploy/grok.env.example /etc/agent-bot/grok.env
sudo cp deploy/codex.env.example /etc/agent-bot/codex.env
sudo cp deploy/antigravity.env.example /etc/agent-bot/antigravity.env

# Restrict each env file to its respective service user
sudo chown root:grok /etc/agent-bot/grok.env && sudo chmod 0640 /etc/agent-bot/grok.env
sudo chown root:codex /etc/agent-bot/codex.env && sudo chmod 0640 /etc/agent-bot/codex.env
sudo chown root:antigravity /etc/agent-bot/antigravity.env && sudo chmod 0640 /etc/agent-bot/antigravity.env
```

Edit `/etc/agent-bot/*.env` with your placeholder-replaced `TELEGRAM_BOT_TOKEN`, `DISCORD_BOT_TOKEN`, and `ALLOWED_USER_IDS` (never commit real secrets or `.env` files).

### 3. Enable and Start `systemd` Template Services

```bash
sudo cp deploy/agent-bot@.service /etc/systemd/system/agent-bot@.service
sudo systemctl daemon-reload

sudo systemctl enable --now agent-bot@grok
sudo systemctl enable --now agent-bot@codex
sudo systemctl enable --now agent-bot@antigravity

# Inspect service health and logs
sudo systemctl status agent-bot@grok agent-bot@codex agent-bot@antigravity
sudo journalctl -u agent-bot@antigravity -f
```

---

## Seamless Migration from `grok-bot` / `codex-bot`

`agent-bot` preserves 100% SQLite schema compatibility (sessions, long-term memory, scheduled tasks, settings), transparently maps legacy sandbox profile names (`grokbot` / `codexbot` -> `agentbot`) when resuming or restoring sessions, and cleans up legacy `# BEGIN GROK-BOT TASKS` / `# BEGIN CODEX-BOT TASKS` crontab blocks automatically:

1. Back up your existing data directory before stopping the legacy service (e.g., `cp -a /var/lib/grok-tg-bot /home/grok/.local/share/agent-bot` and fix ownership `chown -R grok:grok /home/grok/.local/share/agent-bot`).
2. Copy existing settings into `/etc/agent-bot/grok.env` (`BOT_ENGINE=grok`) and `/etc/agent-bot/codex.env` (`BOT_ENGINE=codex`).
3. Disable the legacy unit and enable `agent-bot@grok` / `agent-bot@codex`.
4. Existing scripts calling `$GROK_BOT_BIN send` or `$CODEX_BOT_BIN send` keep working unmodified (`agent-bot` exports `AGENT_BOT_BIN`, `GROK_BOT_BIN`, and `CODEX_BOT_BIN`).

---

## Chat Commands

| Command | Description |
| :--- | :--- |
| `/status` | Show `agent-bot` and backend CLI versions, active model, session, scheduled tasks, auto-upgrade status, and MCP overview |
| `/upgrade` | Check updates and binary write permissions, then return a version-locked upgrade panel |
| `/upgrade run [all\|bot\|cli]` | Run an immediate upgrade without the panel (auto-backs up SQLite, verifies checksums, and restarts in place) |
| `/upgrade auto [on\|off]` | View, enable, or disable idle daily auto-upgrades (default `04:30`) |
| `/model [name]` | Inspect or switch the active model via buttons or arguments |
| `/new [title]` / `/sessions` | Archive the current session and start fresh / browse or hand off sessions across Telegram and Discord |
| `/memory` | Manage cross-platform shared long-term memory (persists across `/reset`) |
| `/task` | Create, test-run, pause, resume, or delete OS-native scheduled tasks via natural language or cron |
| `/mcp` | List, diagnose (`doctor`), add, enable/disable, or remove MCP servers |
| `/usage` / `/quota` | Check remaining subscription allowance windows and recent token usage |
| `/dir` / `/shell` | Manage sandbox writable directory allowlists / toggle server shell execution permission |
| `/stop` / `/reset` | Abort the active turn and queued messages / clear current conversation context |

---

## Development & Verification

```bash
# Run all unit tests
go test -mod=readonly ./...

# Build for the current platform
go build -mod=readonly -trimpath -o agent-bot ./cmd/agent-bot

# Check version and CLI push flags
./agent-bot --version
./agent-bot --help
```
