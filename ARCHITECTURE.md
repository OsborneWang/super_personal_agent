# super_personal_agent — 系统架构图

> 对应 DESIGN.md v0.3。本文档只画图，不重复设计理由。

---

## 1. 总体拓扑

```mermaid
flowchart TB
    subgraph YOU["👤 你"]
        U1["与 Router 对话<br/>（唯一入口）"]
        U2["监视 / 验收<br/>agent ls · show · answer · cancel"]
    end

    subgraph TERMINAL["终端层"]
        R["pi（Router）<br/>交互 TUI · 模型=配置"]
        CLI["agent CLI<br/>（socket 客户端，不碰账本）"]
    end

    subgraph AGENTD["agentd（Go 常驻进程 · 唯一写者）"]
        direction TB
        SOCK["socket 服务<br/>~/.agent/sock"]
        SCHED["调度器<br/>claim ready → spawn worker"]
        TIMER["定时器<br/>租约扫描 · 超时 · 死亡检测"]
        SUP["进程监管<br/>setpgid · heartbeat · SIGTERM"]
        SM["状态机<br/>迁移表 · 单写者"]
        LW["账本写入器<br/>单 goroutine · seq 唯一"]
        NOTIFY["通知器<br/>interrupt / quiet / none"]
    end

    subgraph WORKERS["Worker 层（pi headless 进程）"]
        W1["pi --mode json -p<br/>--session-id T-xxx<br/>Worker L1"]
        W2["子 Worker L2<br/>（继承 session-id / 父模型）"]
        W3["子 Worker L3<br/>（硬性上限）"]
        W1 -->|"拆子任务<br/>产生子 Ticket"| W2
        W2 -->|"再拆"| W3
        W3 -.->|"三层仍未完成<br/>→ waiting_user 预警"| U2
    end

    subgraph STORAGE["存储层"]
        LEDGER[("control/*.jsonl<br/>账本 · 唯一真相<br/>append-only")]
        TRACE[("tickets/&lt;id&gt;/trace.jsonl<br/>pi 事件流 · 证据")]
        RULES["项目规则文件<br/>AGENTS.md 等<br/>（注入 worker 上下文）"]
    end

    U1 <-->|多轮对话 · 草稿确认| R
    U2 --> CLI
    R -->|"file_ticket(spec)<br/>唯一入档动作"| SOCK
    CLI <-->|"admit / ls / show /<br/>follow / answer / cancel"| SOCK

    SOCK --> SM
    SM --> LW
    LW --> LEDGER

    SCHED -->|"claim + 资源互斥"| SM
    SCHED -->|"spawn"| W1
    TIMER --> SUP
    SUP -.->|"worker 死亡 →<br/>ready 重试<br/>（继承 session-id）"| SM
    TIMER -->|"waiting_user 超时"| SM

    W1 -.->|"申报意图<br/>done / failed / need_input"| SOCK
    W1 -.->|"事件流"| TRACE
    RULES -.->|"启动时注入<br/>L2 项目规则"| W1

    SM --> NOTIFY
    NOTIFY -->|"need_input / failed<br/>（critical）"| U2
    NOTIFY -->|"done / closed<br/>（quiet）"| U2
```

---

## 2. 一张 Ticket 的完整生命周期

```mermaid
sequenceDiagram
    autonumber
    participant U as 👤 你
    participant R as Router(pi)
    participant D as agentd
    participant L as 账本(jsonl)
    participant W as Worker(pi)
    participant S as Subagent(pi)

    U->>R: "帮我做 X"
    R->>U: 多轮对话 → 展示草稿 Ticket
    U->>R: "可以"
    R->>D: file_ticket(spec) [unix socket]
    D->>D: 校验 5 条硬约束<br/>(goal/acceptance/context/depth/model)
    D->>L: ticket.admitted
    D-->>R: {ticket_id: T-xxx}
    R-->>U: "已入档 T-xxx" → /new 清空上下文

    Note over D: 调度循环
    D->>D: 资源互斥检查（同 key 独占）
    D->>L: state.changed: pending→ready
    D->>W: spawn: pi --mode json -p<br/>--session-id T-xxx
    D->>L: run.claimed + state.changed: ready→running

    alt Worker 需要问你
        W->>D: need_input("问题 A？")
        D->>L: question.asked + state→waiting_user
        D->>U: 🔔 critical 通知
        U->>D: agent answer T-xxx "答案"
        D->>L: answer.given + state→running
        D->>W: 同 session-id 重新拉起，喂入答案
    end

    alt Worker 拆子任务（≤3层）
        W->>D: 创建子 Ticket（depth+1）
        D->>L: ticket.admitted（子）
        D->>S: spawn 子 Worker<br/>（新 session-id · 继承父模型）
        S-->>D: 子任务 done
        D->>L: state.changed（子）
    end

    alt Worker 死亡（kill -9 / 崩溃）
        D->>D: 租约扫描发现 pid 不存在
        D->>L: state→ready（reason: worker_died_auto_retry）
        D->>W: 新 spawn，同一 session-id<br/>继承上下文继续
    end

    W->>D: done（含验收自检 + 约束处理说明）
    D->>L: run.ended + state→done
    D->>U: 🔔 quiet 通知
    U->>D: 验收 → agent close T-xxx
    D->>D: 两阶段回收资源（标记→等待→SIGTERM→SIGKILL）
    D->>L: resource.released + state→closed
```

---

## 3. 状态机（M0 已实现）

```mermaid
stateDiagram-v2
    [*] --> pending : admit（人工确认）
    pending --> ready : 资源可用
    ready --> running : claim 成功
    running --> waiting_user : need_input
    waiting_user --> running : answer
    running --> ready : worker 死亡<br/>自动重试（继承 session-id）
    running --> done : 验收齐全
    running --> failed : 申报失败 / 三层未完成 / 模型不可用未解决
    waiting_user --> failed : 超时
    done --> closed : 资源回收完
    failed --> closed : 资源回收完
    waiting_user --> closed : cancel
    pending --> closed : cancel
    ready --> closed : cancel
    running --> closed : cancel
    closed --> [*] : 终态（只许 resource.released）
```

---

## 4. 三层约束注入

```mermaid
flowchart LR
    subgraph L1["L1 通用内核（系统强制）"]
        A1["goal 非空"]
        A2["最多三层递归"]
        A3["同 key 资源互斥"]
        A4["closed 终态"]
    end
    subgraph L2["L2 项目规则（注入，不解析）"]
        B1["AGENTS.md"]
        B2["MODULE-CONVENTIONS.md"]
        B3["项目特有约束"]
    end
    subgraph L3["L3 单任务约束（注入，不解析）"]
        C1["不许碰前端"]
        C2["不许 commit"]
        C3["必须写报告"]
    end

    L2 -->|"读入 system prompt<br/>+ 元规则：违反=失败+停+报告"| W["Worker 上下文"]
    L3 -->|"ticket.constraints<br/>随任务下发"| W
    W -->|"报告必须含<br/>每条约束的处理结果"| AUDIT["账本审计"]
    L1 -->|"代码强制<br/>file_ticket 校验 / 迁移表"| SYS["agentd"]
```

---

## 5. 模型与递归（部署决策，不进内核）

```mermaid
flowchart TB
    CFG["~/.agent/config.yaml<br/>model.router / worker / subagent"]
    ADP["项目适配器覆盖<br/>（可选）"]
    TKT["Ticket 覆盖<br/>（Router 对话时决定）"]
    RUN["运行时覆盖<br/>agent run --model"]

    CFG --> ADP --> TKT --> RUN

    R["Router<br/>模型 = 配置"] 
    W1["Worker L1<br/>模型 = ticket 指定"]
    W2["Worker L2<br/>继承父模型（可显式覆盖）"]
    W3["Worker L3<br/>⚠️ 硬上限：未完成 → waiting_user"]

    RUN --> R
    RUN --> W1
    W1 -->|"拆 + 继承"| W2 -->|"拆 + 继承"| W3

    W3 -.->|"模型不可用 → waiting_user<br/>你决定降级或等待"| YOU["👤 你"]
```

---

## 6. 已实现能力

```mermaid
flowchart LR
    A["✅ 账本 + 状态机<br/>重放 + reconcile<br/>kill -9 自愈"]
    B["✅ agentd 常驻<br/>socket + 定时器<br/>资源租约"]
    C["✅ Router 纯分诊<br/>file_ticket + 人工确认"]
    D["✅ 真实 Worker<br/>bash / 联网 / 自检验收"]
    E["✅ need_input 闭环<br/>answer 后同 session 续跑"]
    F["✅ 递归子任务<br/>父等子 + 子结果回填"]
    G["✅ 多项目隔离<br/>配置分层 / 规则隔离"]
    H["✅ systemd 托管<br/>崩溃自动拉起"]

    A --> B --> C --> D --> E --> F --> G --> H
```

详细交互版架构图：**[architecture.html](architecture.html)**
