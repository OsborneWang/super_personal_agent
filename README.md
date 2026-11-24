# super_personal_agent

一个只在终端里用的个人 AI 任务指挥系统。
你只和一个 Router 对话；它把任务写成 Ticket；Worker 执行；Worker 可以自己再拆子 Ticket 递归解决（最多三层）；一个常驻进程 `agentd` 负责调度、计时、监管、记账。

> 设计文档：[DESIGN.md](DESIGN.md) · 架构图：[ARCHITECTURE.md](ARCHITECTURE.md)

---

## 快速开始

```bash
# 1. 构建
cd super_personal_agent
go build -o agent .

# 2. （可选）配置模型与项目规则
cat > ~/.agent/config.yaml <<'EOF'
model:
  router: ""        # 你用的 Router 模型（留空=pi 当前模型）
  worker: ""        # Worker 默认模型（留空=pi 当前模型）
  subagent: inherit # 子 agent 继承父模型
rules: []           # L2 项目规则文件，会被注入每个 Worker 的上下文
#  - <project>/AGENTS.md
EOF

# 3. 启动后台进程
./agent daemon
```

启动后，Router 会话（pi）里会自动加载 `file_ticket` 工具。
你和 Router 聊出一张草稿、确认后，任务就进入系统，Worker 自动开始执行。

---

## 你在终端里用的命令

| 命令 | 作用 |
|---|---|
| `agent daemon` | 启动后台进程（前台运行，Ctrl+C 停止） |
| `agent ls` | 管理视角：所有 Ticket 的状态 |
| `agent show <id>` | 单张 Ticket 的细节 |
| `agent follow <id>` | 实时盯住一张 Ticket 的状态变化 |
| `agent answer <id> <text>` | 回答 Worker 抛给你的问题 |
| `agent cancel <id>` | 强制终止一张 Ticket |
| `agent ping` | 健康检查 |

---

## 一张 Ticket 的一生

```
你 ⇄ Router（多轮对话）
      → 草稿确认（工具内人工门禁，模型伪造不了）
      → file_ticket → agentd 校验 5 条硬约束
      → pending → ready（资源可用）→ running（spawn pi worker）
      → Worker 干活（事件流写入 trace.jsonl）
      → 三种结局：
          done          → 通知你验收
          need_input    → 通知你 → 你 answer → 同 session-id 续跑
          failed        → 通知你
      → 你验收 → closed（资源两阶段回收）
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
| **L2 项目规则** | 项目特有的约束（如"禁主库""必须用某脚本"） | 注入 Worker 上下文，不解析 |
| **L3 单任务约束** | 这张票不许碰前端、必须写报告等 | 注入 Worker 上下文，不解析 |

系统管"规则必须在场 + 结果必须交代"，不管"规则是什么"。
所以换项目不用改内核——改 `config.yaml` 的 `rules` 列表即可。

---

## 验证过的属性

- ✅ **kill -9 后自愈**：reconcile 检测孤儿 worker，同 session-id 重试，无死亡检测死循环
- ✅ **状态机强制**：非法迁移被拒绝（closed 是终态）
- ✅ **真实 Worker 端到端**：写文件、执行命令、逐条自检验收、输出控制标记
- ✅ **need_input 闭环**：Worker 停下问 → 你回答 → 同 session-id 续跑（继承上下文）
- ✅ **递归子任务**：父拆子、父等子、子继承父模型 + 独立 session-id
- ✅ **资源互斥**：同 key 资源独占，冲突任务排队等待，释放后自动接续
- ✅ **并发调度**：多个真实 Worker 并行执行
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
| `daemon.go` | 常驻进程、调度循环、watchdog、answer 恢复 |
| `server.go` | unix socket 服务、命令分发、file_ticket 5 条硬校验 |
| `client.go` | socket 客户端 |
| `worker.go` | spawn pi、事件流落 trace、结局分类（done/failed/need_input） |
| `config.go` | `~/.agent/config.yaml` 加载 |
| `notify.go` | 三级通知（interrupt/quiet/none） |

Router 扩展（pi）：`~/.pi/agent/extensions/super-agent-router/index.ts`
注册 `file_ticket` 工具（唯一入档入口，含人工确认门禁）。

---

## 数据布局

```
~/.agent/
  sock                          # agentd 的 unix socket
  config.yaml                   # 你的部署配置（模型/规则）
  control/YYYY-MM-DD.jsonl      # 控制账本（唯一真相，append-only）
  tickets/<id>/
    trace.jsonl                 # Worker 的 pi 事件流（证据，可审计）
    .sysprompt.txt              # 注入该 Worker 的系统提示（L2 规则）
    work/                       # Worker 的工作目录（沙箱）
```

---

## 还没做（里程碑路线）

- [ ] **M2 完整化**：Router 的 `/tickets` 命令、草稿持久化（draft.opened/amended）
- [ ] **waiting_user 超时**：已实现但超时阈值需按使用习惯调优（默认 30 分钟，`config.yaml` 的 `waiting_timeout_minutes`）
- [ ] **通知分级调优**：基于 `notification.sent` 事件统计打扰频率
- [ ] **资源回收的 worktree/端口实体清理**：目前回收的是进程组和资源租约；真实的 `git worktree remove` / 端口释放由适配器实现

## 已完成并验证

- [x] **资源互斥**：`resources` 同 key 独占，调度器真实执行锁，冲突排队
- [x] **递归子任务**：Worker 通过 `create_subtask` 拆子任务，父等子，子继承父模型 + 独立 session-id
- [x] **三层递归硬限制**：到达上限触发 waiting_user 人工介入
- [x] **waiting_user 超时**：超时自动 failed 并通知
- [x] **最大尝试次数**：worker 死亡重试超限自动 failed
- [x] **资源回收**：cancel/close 时两阶段回收（SIGTERM→SIGKILL 进程组 + 释放租约）

当前 M0–M5 主流程全部验证通过。
