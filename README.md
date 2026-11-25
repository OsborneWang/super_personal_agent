# super_personal_agent（简称 sa）

一个只在终端里用的个人 AI 任务指挥系统。
你只和一个 Router 对话；它把任务写成 Ticket；Worker 执行；Worker 可以自己再拆子 Ticket 递归解决（最多三层）；一个常驻后台服务 `sa`（systemd 托管）负责调度、计时、监管、记账。

**支持多项目并行**：一个全局服务，可在任意多个项目目录里发起任务，Worker 在对应项目目录里干活、用对应项目的规则和模型，但你能在同一个看板看到所有任务。

> 设计文档：[DESIGN.md](DESIGN.md) · 架构图：[ARCHITECTURE.md](ARCHITECTURE.md)

---

## `sa` 和 `pi` 的区别（核心心智模型）

| 命令 | 是什么 | 什么时候用 |
|---|---|---|
| **`sa`** | 打开一个 **Router 会话**，工作进入任务系统：你确认的任务会被 Worker 在后台执行、被跟踪、可被监视和验收 | 你要"指挥 AI 干活"，尤其是**多个并行任务**、**后台执行**、**需要监视进度**、**需要可追溯** |
| **`pi`** | 单纯的编码助手，没有任何任务系统：没有 file_ticket、不会调度 Worker、不留账本 | 临时问个问题、改个文件、一次性的小活儿，**不需要跟踪也不需要并行** |

一句话：**`sa` 用来指挥，`pi` 用来随手干活。** 判断标准——这件事值不值得被跟踪和管理？值得就 `sa`，不值得就 `pi`。

---

## 启动服务（三步）

### 1. 构建并安装到 PATH

```bash
cd /path/to/super_personal_agent
go build -o agent .

# 把 agent 装进 PATH（符号链接，重build自动生效；sa 脚本依赖它）
ln -sf "$PWD/agent" ~/.local/bin/agent
# sa Router 启动器（已存在于 ~/.local/bin/sa；没有的话见下文）
```

> `sa` 和 `agent` 都放在 `~/.local/bin/`（该目录已在 PATH）。`agent` 用符号链接指向 build 产物，所以你每次 `go build` 后无需重新安装。
>
> 如果 `~/.local/bin/sa` 不存在，把仓库里的 `sa` 启动器链接过去：
> `ln -sf "$PWD/packaging/sa" ~/.local/bin/sa`（或按下方"sa 启动器"一节创建）。

### 2. 启动后台服务（一次性，systemd 托管）

```bash
sa start
```

第一次运行会自动安装一个 systemd **用户服务**（`super-agent.service`）并启动。之后它就常驻后台、由 systemd 托管，**你不用再开窗口守着它**：

- **崩溃自动拉起**：kill -9 之后 systemd 会 3 秒内重启它（`Restart=on-failure`）
- **登录自启**：`sa enable` 后，每次登录自动启动
- **状态查看**：`sa status`（等价于 `systemctl --user status super-agent.service`）

服务管理：

```bash
sa status          # 查看运行状态
sa logs [-f]       # 跟踪日志
sa stop / restart  # 停止 / 重启
sa enable          # 登录自启
sa uninstall       # 卸载服务
```

> **说明**：worker 需要执行 `pi`（一个依赖 node 的脚本），而 systemd 环境的 PATH 默认不含它。`install` 时会自动探测 `pi` 和 `node` 的实际路径注入到服务里，无需手工配 PATH。
>
> **进阶**：希望 daemon 在**完全登出**（没有任何登录会话）后也继续跑，执行一次 `loginctl enable-linger $USER`。

### 3. 开一个 Router 会话开始指挥

```bash
sa        # 开 Router 会话（自动带任务系统）
```

然后正常聊天：`帮我做 X`。Router 会跟你多轮确认需求、弹出草稿让你确认，确认后任务自动进入系统、Worker 在后台开跑。

**监视进度**（另开一个终端）：

```bash
sa ls            # 看板：所有任务（按项目分组）
sa follow <id>   # 实时盯住某个任务
```

---

## 多项目使用

daemon 是全局唯一的，你可以在**任意多个项目目录**开 pi 发起任务。
任务在哪个目录发起，就归属哪个项目：Worker 在那个项目目录里干活、用那个项目的规则和模型。

### 项目级配置（覆盖全局）

在项目目录下放 `.agent/config.yaml`：

```yaml
# <project>/.agent/config.yaml
model:
  worker: ""          # 这个项目 Worker 用的模型（覆盖全局）
rules:
  - <project>/AGENTS.md      # 项目规则，注入该项目所有 Worker
  - <project>/MODULE-CONVENTIONS.md
waiting_timeout_minutes: 60               # 这个项目 waiting_user 超时时间
max_attempts: 3                            # worker 死亡重试上限
```

**优先级**：项目配置 > 全局配置（`~/.agent/config.yaml`）。标量字段（模型、超时、重试次数）项目覆盖全局；**规则（rules）是拼接**——全局和项目规则都会注入 Worker。

### 全局配置（兜底）

`~/.agent/config.yaml`（可选，没有也行）：

```yaml
model:
  router: ""        # Router 模型（留空 = pi 当前模型）
  worker: ""        # Worker 默认模型（留空 = pi 当前模型）
  subagent: inherit # 子 agent 继承父模型
rules: []           # 全局规则，所有项目的 Worker 都会注入
```

---

## 你在终端里用的命令

日常你只用一个入口 **`sa`**；底层 `agent` 命令也存在（`sa` 是它的包装）。

| 命令 | 作用 |
|---|---|
| `sa` | 开 Router 会话（进入任务系统） |
| `sa start` / `stop` / `restart` | 后台服务控制（systemd） |
| `sa status` / `sa logs [-f]` | 服务状态 / 日志 |
| `sa enable` / `sa uninstall` | 登录自启 / 卸载 |
| `sa ls` | **看板**：所有 Ticket 状态，按项目分组 |
| `sa show <id>` | 单张 Ticket 的细节 |
| `sa follow <id>` | 实时盯住一张 Ticket 的状态变化 |
| `sa answer <id> <text>` | 回答 Worker 抛给你的问题 |
| `sa cancel <id>` | 强制终止一张 Ticket（回收进程组 + 资源） |
| `sa ping` | 健康检查（服务是否在跑） |
| `sa --help` | sa 自身说明（含 sa/pi 区别） |

---

## 一张 Ticket 的一生

```
你在某个项目目录 ⇄ Router（多轮对话）
      → 草稿确认（工具内人工门禁，模型伪造不了）
      → file_ticket（自动带上 project = 当前目录）
      → agentd 校验 5 条硬约束
      → pending → ready（资源互斥检查）→ running（在项目目录 spawn worker）
      → Worker 干活（事件流写入 <项目>/.agent/tickets/<id>/trace.jsonl）
      → 三种结局：
          done          → 通知你验收
          need_input    → 通知你 → 你 answer → 同 session-id 续跑
          failed        → 通知你
      → 你验收 → closed（回收进程组 + 释放资源租约）
```

Worker 死亡 / agentd 被 kill -9：重启后 reconcile 自动发现孤儿，转 ready 用同一 session-id 重试，不丢上下文。

---

## 系统怎么管 Worker（而不是替它思考）

`agentd` 只做确定性的事，所有"判断"交给 Worker（pi）：

- **调度骨架（agentd，Go）**：socket 服务、账本 append、状态机裁决、定时器、spawn/kill 进程、租约、资源互斥、发通知
- **认知工作（Worker，pi）**：理解任务、拆分、调用工具、判断何时算完成、遇到阻塞何时停下来问

**状态只由 agentd 写；Worker 只能"申报意图"**（done / failed / need_input），最终状态由状态机查表裁决。模型幻觉不会污染状态。

---

## 三层约束（通用性的核心）

| 层 | 内容 | 内核处理 |
|---|---|---|
| **L1 通用内核** | goal 非空、最多三层递归、同 key 资源互斥、closed 终态 | 系统强制 |
| **L2 项目规则** | 项目特有的约束（如"禁主库""必须用某脚本"） | 注入该项目 Worker 上下文，不解析 |
| **L3 单任务约束** | 这张票不许碰前端、必须写报告等 | 注入该 Worker 上下文，不解析 |

系统管"规则必须在场 + 结果必须交代"，不管"规则是什么"。
规则跟着项目走（项目 config 的 `rules`），所以不同项目的 Worker 各用各的规则，互不冲突。

---

## 数据布局

```
~/.agent/                              ← 全局：系统的运行记录
  sock                                  # daemon 的 unix socket（全局唯一）
  config.yaml                           # 全局配置（兜底）
  control/YYYY-MM-DD.jsonl              # 控制账本（唯一真相，append-only，含所有项目）

<项目目录>/.agent/                      ← 每个项目：物理工作区
  config.yaml                           # 项目配置（覆盖全局）
  tickets/<id>/
    trace.jsonl                         # 该项目 Worker 的事件流（证据，可审计）
    .sysprompt.txt                      # 注入该 Worker 的系统提示（L2 规则）
    work/                               # Worker 的工作目录（能直接读写项目文件）
```

**关键**：Worker 在 `<项目目录>/.agent/tickets/<id>/work/` 里跑，cwd 就是项目内的沙箱，能直接用相对/绝对路径访问项目文件。资源 key 自动加项目前缀，两个项目用同名资源不冲突。

---

## 验证过的属性

- ✅ **多项目并行**：一个 daemon，多项目目录发起，Worker 在对应项目目录干活，零交叉污染
- ✅ **规则隔离**：项目 A 的规则只注入 A 的 Worker，不泄漏到 B
- ✅ **配置分层**：项目配置覆盖全局（模型/超时/重试），规则拼接
- ✅ **资源命名空间**：资源 key 自动加项目前缀，跨项目同名资源不冲突
- ✅ **kill -9 后自愈**：reconcile 检测孤儿 worker，同 session-id 重试，无死循环
- ✅ **状态机强制**：非法迁移被拒绝（closed 是终态）
- ✅ **真实 Worker 端到端**：写文件、执行命令、逐条自检验收、输出控制标记
- ✅ **need_input 闭环**：Worker 停下问 → 你回答 → 同 session-id 续跑（继承上下文）
- ✅ **递归子任务**：父拆子、父等子（子结果回填）、子继承父模型 + 独立 session-id
- ✅ **资源互斥**：同 key 资源独占，冲突任务排队等待，释放后自动接续
- ✅ **取消回收**：cancel 时 SIGTERM→SIGKILL 进程组 + 释放资源租约
- ✅ **诚实性**：Worker 遇到无法满足的前提会拒绝编造，如实报告
- ✅ **账本完整**：append-only，零 torn write，可 grep、可审计

---

## 代码结构

| 文件 | 职责 |
|---|---|
| `main.go` | CLI 入口（daemon / ls / show / follow / answer / cancel / test） |
| `ledger.go` | append-only 账本、高水位恢复、不完整行截断 |
| `state.go` | 状态机（迁移表强制）、replay、Ticket 模型 |
| `reconcile.go` | 启动自愈、孤儿检测（/proc 判活） |
| `daemon.go` | 常驻进程、调度循环、watchdog、answer/子任务恢复、配置分层、资源回收 |
| `server.go` | unix socket 服务、命令分发、file_ticket 5 条硬校验 |
| `client.go` | socket 客户端 |
| `worker.go` | spawn pi（按项目目录）、事件流落 trace、结局分类 |
| `config.go` | 全局/项目配置加载与合并（项目优先） |
| `resource.go` | 资源租约管理（同 key 互斥） |
| `notify.go` | 三级通知（interrupt/quiet/none） |

pi 扩展：
- Router：`~/.pi/agent/extensions/super-agent-router/index.ts`（`file_ticket` 工具，含人工确认门禁）
- Worker：`~/.pi/agent/extensions/super-agent-worker/index.ts`（`create_subtask` 工具，仅在 worker 环境注册）

---

## 还没做（里程碑路线）

- [ ] **M2 完整化**：Router 的 `/tickets` 命令、草稿持久化（draft.opened/amended）
- [ ] **waiting_user 超时**：已实现，阈值需按使用习惯调优（项目 config 的 `waiting_timeout_minutes`）
- [ ] **通知分级调优**：基于 `notification.sent` 事件统计打扰频率
- [ ] **资源回收的实体清理**：目前回收进程组和资源租约；真实的 `git worktree remove` / 端口释放由项目适配器实现
- [ ] **daemon 后台化管理**：`daemon start/stop/status/logs`，免去手动开窗口（见下方说明）

---

## 关于 daemon 的使用体验

`agent daemon start` 之后 daemon 由 systemd 托管，**没有交互输出是正常的**（它在后台调度）。
- 看状态：`agent daemon status`
- 看日志：`agent daemon logs -f`
- 崩溃自愈：kill -9 后 systemd 自动拉起，且 reconcile 恢复任务状态
- 完全登出也活着：`loginctl enable-linger $USER`

你日常只需要：`agent daemon start`（一次）→ 开 pi 对话 → `agent ls` 看进度。
