# 安装（把仓库里的东西接入系统）

本目录存放"运行环境接入"所需的东西：`sa` 启动器、Router 系统提示词、两个 pi 扩展。

## 一次性安装

```bash
cd /path/to/super_personal_agent

# 1. 构建二进制
go build -o agent .

# 2. 把 agent 和 sa 放进 PATH（符号链接，重 build 自动生效）
ln -sf "$PWD/agent"                ~/.local/bin/agent
ln -sf "$PWD/packaging/sa"         ~/.local/bin/sa

# 3. 安装两个 pi 扩展（Router 侧 file_ticket / Worker 侧 create_subtask）
mkdir -p ~/.pi/agent/extensions
ln -sfn "$PWD/packaging/pi-extensions/super-agent-router" ~/.pi/agent/extensions/super-agent-router
ln -sfn "$PWD/packaging/pi-extensions/super-agent-worker" ~/.pi/agent/extensions/super-agent-worker

# 4. 安装并启动 systemd 用户服务
sa install
sa start

# 5.（可选）登录自启
sa enable
```

## 验证

```bash
sa ping              # 后台服务活着
sa status            # systemd 状态
sa --help            # 命令一览
```

## 两个扩展各自的作用

| 扩展 | 何时生效 | 作用 |
|---|---|---|
| `super-agent-router` | 仅当 `SA_MODE=1`（即通过 `sa` 启动） | 提供 `file_ticket` 工具，是任务进入系统的唯一入口；`pi` 启动时完全不加载 |
| `super-agent-worker` | 仅当 `SUPER_TICKET_ID` 环境变量存在（即由 daemon spawn 的 worker） | 提供 `create_subtask` 工具，让 worker 能拆子任务 |

## 注意

- 这两个扩展是**软链接**进 `~/.pi/agent/extensions/` 的，所以改仓库里的代码即刻生效，无需重复安装。
- `sa` 依赖 `packaging/router-system-prompt.txt`（Router 角色提示词，定义"传话筒"行为）。该文件在仓库里，`sa` 通过 `SA_HOME` 定位（默认 `/path/to/super_personal_agent`）。如果你把仓库放在别的位置，需要设置 `SA_HOME` 环境变量。
