# super_personal_agent — 设计 v0.3

> **一句话**：一个只在终端里用的个人任务系统。
> 你只和一个 Router 对话；它把任务写成 Ticket；Worker 执行；
> Worker 可以自己再拆子 Ticket 递归解决（最多三层）；
> 一个常驻进程 `agentd` 负责调度、计时、监管、记账。

---

## 0. 三个决定性判断（通用内核的边界）

| # | 判断 | 理由 |
|---|---|---|
| J1 | **Router 是交互 pi 会话，人驱动；`agentd` 是后台进程，机器驱动** | 两个进程、两种节奏、两种信任级别 |
| J2 | **状态只由 `agentd` 写；pi 只能"申报意图"** | 单写者 + 代码校验 = 模型幻觉不污染状态 |
| J3 | **调度骨架不给 LLM，认知工作全给 LLM** | 定时器、租约、进程回收必须确定；判断、拆分、重试策略交给 pi |

**通用性检验**：同一内核、零代码改动，能同时跑「查今天的天气」（深度1）和「<big-change> S0→S3 + 三个仓 + 一个隔离库」（深度2，含跨任务依赖）。

---

## 1. 组件与边界

```
┌──────────────────────────────────────────────────────────────┐
│  你在终端里                                                    │
│                                                              │
│   pi（Router，交互 TUI）                                       │
│     ├─ 多轮对话，逐步问清需求                                    │
│     ├─ 在聊天里展示草稿 Ticket                                   │
│     ├─ 调用工具 file_ticket(spec)  ←── 唯一入档动作              │
│     └─ 入档成功后 /new，上下文清空                               │
│                    │                                          │
│                    │ unix socket                              │
│   agent CLI ───────┤  （全部都是 socket 客户端，不碰账本）        │
│     ls / show / follow / answer / cancel / drafts             │
└────────────────────┼──────────────────────────────────────────┘
                     ▼
              ~/.agent/sock
                     │
┌────────────────────▼──────────────────────────────────────────┐
│  agentd（Go，常驻，PID 唯一，唯一写者）                          │
│                                                              │
│   socket 服务 ── 命令：admit/list/show/follow/answer/cancel    │
│   账本写入器 ── 单 goroutine，append-only，seq 唯一分配          │
│   内存投影   ── tickets / questions / resources / leases       │
│   定时器     ── 租约到期、User_Waiting 超时、GC                 │
│   进程监管   ── spawn pi、setpgid、heartbeat、SIGTERM 回收      │
│   通知器     ── 三级通道，去重                                   │
│                    │                                          │
│                    │ spawn / stdin / stdout                    │
│                    ▼                                          │
│   pi --mode json -p --session-id T-xxxxx --model <config>     │
│     └─ subagent tool → pi --mode json -p --no-session         │
│         （继承父模型，继承 session-id）                          │
└──────────────────────────────────────────────────────────────┘
```

### 各自绝对不做的事

- **Router 不写账本。** 只能调用工具；工具由 `agentd` 校验后写入。
- **`agentd` 不思考。** 不知道"这个任务该怎么办"，只执行 spec 里的约束。
- **Worker 不改自己的状态。** 只能申报 `done / failed / need_input`，最终状态由 `agentd` 裁决。
- **谁都不能改历史。** 账本只追加；纠错靠追加新事件。

---

## 2. 核心模型：递归任务树（最多三层）

**没有"一次性/复杂"两种任务，只有同一套 Ticket 递归。**

- **Router 只做一次拆分**：把"要做的事"整包变成一张 Ticket，最多拆成几张并列的。不做阶段规划。
- **Worker 是递归点**：拿到 Ticket 后自己判断"能一个人做完，还是要再拆"。拆了就产生**子 Ticket**，子 Ticket 走同一套"认领 → 执行 → 结果"循环。
- **硬性限制：最多三层递归**（Worker 是第一层，允许拆到第三层）。

### 防 AI 走偏：三层限制 + 人工介入

```yaml
depth: 1  # 当前层级，1=Worker，2=子任务，3=子任务的子任务
max_depth: 3  # 硬限制，不可超过
```

**到达第三层还不能完成 → 状态转 `waiting_user`，向你预警**：
- 可能是 AI 走偏了
- 可能是需求理解有问题
- 必须人工介入判断

**这是硬性限制，不是预算约束**——系统强制，worker 无法绕过。

---

## 3. 规则注入：三层约束模型

**约束的载体是"项目规则"，不是"系统机制"。**

| 层 | 例子 | 属于谁 | 内核处理 |
|---|---|---|---|
| **L1 通用内核** | 任务必须有 goal；最多三层递归；同 key 资源互斥 | 所有项目共有 | ✅ 强制 |
| **L2 项目规则** | <project> 禁主库、必须用 `scripts/slug`、禁止改冻结区 | 单个项目 | ❌ 不解析，只注入 |
| **L3 单任务约束** | 这张 ticket 不许碰前端、不许 commit、必须写报告 | 单个任务 | ❌ 不解析，只注入 |

### 机制

1. **适配器声明**：`rules = [/path/to/AGENTS.md, ...]`
2. **Worker 启动时注入**：把规则文件内容写入 system prompt，加元规则：
   > "以下规则是项目权威约束，违反任何一条都视为任务失败，必须立即停止并报告。"
3. **系统不解析、不检查规则内容**——那是 worker 的认知工作。
4. **系统强制两件事**：
   - Worker 每次运行都必须带这些规则
   - Worker 停止时必须在报告里说明每条约束的处理结果（哪怕写"不适用"）

**系统管"规则必须在场"和"结果必须交代"，不管"规则是什么"。**

---

## 4. 模型配置：三层可覆盖

**模型选择是部署决策，不是架构决策。** 系统只定义加载顺序和覆盖规则，具体用什么模型由配置文件决定。

### 加载顺序（后者覆盖前者）

1. **全局默认**：`~/.agent/config.yaml` 里的 `model.router` / `model.worker` / `model.subagent`
2. **项目覆盖**：项目适配器可以声明 `model.worker`
3. **Ticket 覆盖**：Router 在对话时判断"这活值多少钱"，写进 ticket 的 `model.worker`
4. **运行时覆盖**：`agent run --model xxx` 临时指定

**Ticket 覆盖是必需**：Router 在对话时就决定好模型，写进 ticket。

### 模型不可用的处理

Worker 启动时发现指定模型不可用（API key 失效/配额满）：
- **状态转 `waiting_user`**
- 向你报告"模型 X 不可用，是否降级到 Y 或等待恢复"
- **你决定**，worker 不自作主张降级

### Subagent 继承规则

**Subagent 默认继承父模型。** 简单、可预测。

Worker 可以在拆子任务时显式覆盖，但覆盖必须写进子 ticket 的 `constraints`，账本里能看到"为什么这个 subagent 用了贵模型"。

### 成本不账本化

**已有外部 token 监管平台**，系统不重复记录 token 消耗。账本只记"用了什么模型"，不记"花了多少钱"。

---

## 5. 资源互斥：适配器提供的约束

**资源不是内核功能，是适配器声明的约束。**

```yaml
resources:
  - kind: worktree
    key: <project>:worktree:<big-change>
    mode: exclusive
  - kind: port
    key: <project>:port:8060
    mode: exclusive
  - kind: port
    key: <project>:port:8061
    mode: exclusive
```

**内核只负责"同一 key 同一时刻只有一个持有者"，不关心 worktree 是什么。**

- <project> 适配器提供 `worktree` 和 `port` 两类资源
- 换个项目适配器提供 `gpu` / `test-vm` / `license`
- 内核一行不改

**资源 key 带命名空间**：`<project>:worktree:<big-change>`，不同项目资源不撞名。

---

## 6. 状态机：简化版

**区分"等资源"和"在运行"，删除依赖概念。**

```
        ┌─────────┐
        │  draft  │  （不在 ticket 状态机内）
        └────┬────┘
             │ admit（人工确认）
             ▼
        ┌─────────┐  资源可用   ┌─────────┐
        │ pending │───────────►│  ready  │
        └─────────┘            └────┬────┘
             ▲                      │ claim 成功
             │                      ▼
             │                 ┌─────────┐
             │   worker 死亡    │ running │
             │   自动重试      └────┬────┘
             │                      │
             │                      ├── need_input ──►┌──────────────┐
             │                      │                 │ waiting_user │
             │                      │◄──── answer ───└──────────────┘
             │                      │                   （超时 → failed）
             │                      ├── done ────►┌──────┐
             │                      └── failed ──►│ done │ / │failed│
             │                                    └──┬───┘
             │                                       │ close（GC 完）
             │                                       ▼
             │                                  ┌────────┐
             └──── cancel（任意非终态）──────────►│ closed │
                                                └────────┘
```

| from | to | 唯一执行者 | 条件 |
|---|---|---|---|
| pending | ready | agentd | 资源可用 |
| ready | running | agentd | 原子 claim 成功 |
| running | waiting_user | agentd | worker 申报 need_input 且租约有效 |
| waiting_user | running | agentd | 收到 answer |
| running | done | agentd | worker 申报 done 且验收字段齐全 |
| running | failed | agentd | worker 申报 failed / 三层递归未完成 / 模型不可用未解决 |
| running | ready | agentd | worker 死亡，自动重试（继承 session-id） |
| done/failed/waiting_user | closed | agentd | 资源已释放（两阶段） |
| 任意非 closed | closed | agentd | 人 cancel |

**三条铁律：**

1. `closed` 是终态。到达后除 `resource.released` 外**不允许任何事件**；违反则告警。
2. 状态迁移只有 `agentd` 能写，`by.actor` 永远是 `agentd`，`data.reason` 记录触发者。
3. 迁移必须查表，代码里不许出现裸的 `state = "done"`。

---

## 7. 数据模型：Ticket Spec（入档后冻结）

```yaml
id: T-261124-7f3a
revision: 1
admitted_at: 2026-11-24T11:50:02+08:00
admitted_by: user

goal: 查清今天的天气，并给出一句穿衣建议

acceptance:
  - 给出当前温度与体感温度
  - 给出今日降水概率
  - 给出一句穿衣建议
  - 数据来源可追溯

context:
  user_intent: 用户明早要出门，想知道穿什么
  known_facts: ["用户在上海"]
  not_required: ["不需要未来多日预报"]

constraints:
  workspace: ~/.agent/tickets/T-261124-7f3a/work
  rules: []  # L2 项目规则，由适配器注入
  task_constraints: []  # L3 单任务约束

depth: 1  # 当前层级
max_depth: 3  # 硬限制

resources:
  - kind: worktree
    key: <project>:worktree:<big-change>
    mode: exclusive

model:
  # 具体模型名由配置文件决定
  # router: <由 ~/.agent/config.yaml 或项目适配器决定>
  # worker: <由 ~/.agent/config.yaml 或项目适配器决定，可被 ticket 覆盖>
  subagent: inherit  # 继承父模型，可被显式覆盖

notify:
  on_need_input: interrupt
  on_done: quiet
  on_failed: interrupt
```

**硬约束（由 `file_ticket` 工具强制校验）：**

1. `goal` 非空且 > 10 字。
2. `acceptance` 至少 1 条。
3. `context` 至少含 `user_intent` 或 `known_facts` 之一。
4. `depth` 必须 ≤ `max_depth`。
5. `model.worker` 必须是已知模型名。

> 校验失败 → 工具返回结构化错误 → Router 必须回去继续问人。

---

## 8. Session-ID 继承规则

**每个任务绑定一个 session-id，失败重试不丢上下文。**

- Worker 拆子任务时，每个子任务有自己的 ticket id 和 session-id
- 子任务失败重试时，**用同一个 session-id 启动新 agent**，继承之前的上下文
- Worker 进程死亡，`agentd` reconcile 时检测到 pid 不存在：
  - **新建一个 worker，用同一个 session-id 启动，继续工作**
  - 状态从 `running` 转 `ready`，再由调度器重新 claim

**这样 worker 崩溃后不会从零开始，而是从上次中断的地方继续。**

---

## 9. 接口只有两类

### 人对系统（只通过 Router）

- 创建任务
- 查看状态（`agent ls` / `agent show` / `agent follow`）
- 回答问题（`agent answer T1 "..."`）
- 取消任务（`agent cancel T1`）
- 验收结果

### 系统内部（Ticket 流转）

- Router → Worker：分配 Ticket
- Worker → Subagent：拆分子 Ticket（递归，最多三层）
- Worker → agentd：申报意图（done / failed / need_input）
- agentd → 人：通知（三级通道）

**没有旁路。** 这是"只对接 Router"的系统强制。

---

## 10. 监视与验收：你的专属权利

**看板、gate 审批，这些接口只对你开放，不给任何 agent。**

- `agent ls` —— 粗表（管理视角）
- `agent show T1` —— 细账（工作视角）
- `agent follow T1` —— 实时流
- `agent answer T1 "..."` —— 回答门禁
- `agent cancel T1` —— 强制终止

**"最终由我来验收"是系统强制，不是约定。**

---

## 11. 通知分级

| 级别 | 触发 | 通道 | 去重 |
|---|---|---|---|
| interrupt | `question.asked`、`failed` | `notify-send -u critical` + 响铃 | 每 question_id 一次 |
| quiet | `done`、`closed` | `notify-send` 静音 | 每 ticket 一次 |
| none | 其余所有迁移 | 只落账 | — |

---

## 12. 里程碑

| # | 交付 | 通过标准 |
|---|---|---|
| M0 | 账本 + 状态表 + 重放 + reconcile | 手写事件能重建状态；`kill -9` 后自愈；worker 死亡自动重试 |
| M1 | agentd 常驻 + socket + 定时器 + 租约 | 假 worker（sleep 脚本）能跑完一条 ticket；杀了能重排 |
| M2 | file_ticket 工具 + Router 扩展 | 聊天 → 草稿 → 确认 → 入档 → `/new` 全通 |
| M3 | 接 pi worker + trace 落账 + 三层限制 | 真实任务端到端；三层递归未完成转 waiting_user |
| M4 | ask_user + waiting_user + 三级通知 | 后台跑到一半叫你，你回答后继续 |
| M5 | 资源互斥 + 子 agent 继承 session-id | 同 key 资源互斥；失败重试用同 session-id |

---

## 13. 愿景：super-agent

**最终形态**：这个系统自己能造出另一个这样的系统，并且能指挥它。

**现在不写代码，只写愿景。**

**当前边界**：
- 你只对接 Router
- 后续工作你只要能监视状态和进度
- 最终由你验收

这就是 v0.3 的全部。
