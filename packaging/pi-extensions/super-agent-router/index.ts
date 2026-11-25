/**
 * super-agent-router — Router extension for super_personal_agent
 *
 * Registers the `file_ticket` tool: the ONLY way a ticket enters the system.
 * Flow: model builds spec → tool renders draft → ctx.ui.confirm (human gate,
 * cannot be forged by the model) → unix socket to agentd → returns ticket_id.
 *
 * The Router never writes the ledger itself; agentd validates + writes.
 */

import { Type } from "@earendil-works/pi-ai";
import { StringEnum } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { connect } from "node:net";
import { homedir } from "node:os";
import { join } from "node:path";

// ---------- socket client ----------

interface AgentdResponse {
	id?: string;
	success: boolean;
	error?: string;
	data?: unknown;
}

function socketPath(): string {
	return join(homedir(), ".agent", "sock");
}

async function callAgentd(command: string, args: Record<string, unknown>): Promise<AgentdResponse> {
	return new Promise((resolve, reject) => {
		const conn = connect(socketPath());
		const chunks: Buffer[] = [];
		let settled = false;

		const fail = (err: Error) => {
			if (!settled) {
				settled = true;
				conn.destroy();
				reject(err);
			}
		};

		conn.setTimeout(8000);
		conn.on("timeout", () => fail(new Error("agentd timeout (is it running? try `agent daemon`)")));
		conn.on("error", (err) => fail(new Error(`connect agentd: ${err.message}`)));
		conn.on("data", (d) => chunks.push(d));
		conn.on("end", () => {
			if (settled) return;
			settled = true;
			try {
				const text = Buffer.concat(chunks).toString("utf8");
				const line = text.split("\n").find((l) => l.trim().length > 0) ?? "{}";
				resolve(JSON.parse(line) as AgentdResponse);
			} catch (err) {
				reject(new Error(`parse agentd response: ${String(err)}`));
			}
		});
		conn.on("connect", () => {
			const req = JSON.stringify({ id: "r1", command, args }) + "\n";
			conn.write(req, (err) => {
				if (err) fail(new Error(`write to agentd: ${err.message}`));
				else conn.end(); // half-close: signal we're done writing
			});
		});
	});
}

// ---------- draft rendering ----------

interface TicketSpec {
	goal: string;
	acceptance: string[];
	context?: Record<string, unknown>;
	depth?: number;
	max_depth?: number;
	parent_id?: string;
	model?: { worker?: string };
	notify?: Record<string, string>;
	constraints?: Record<string, unknown>;
}

function renderDraft(spec: TicketSpec): string {
	const lines: string[] = [];
	lines.push("┌─ 草稿 Ticket ─────────────────────────────");
	lines.push(`│ 目标: ${spec.goal}`);
	lines.push("│ 验收标准:");
	for (const a of spec.acceptance) lines.push(`│   - ${a}`);
	if (spec.context && Object.keys(spec.context).length > 0) {
		lines.push("│ 上下文:");
		for (const [k, v] of Object.entries(spec.context)) {
			const val = Array.isArray(v) ? v.join(", ") : String(v);
			lines.push(`│   ${k}: ${val}`);
		}
	}
	lines.push(`│ 递归深度: ${spec.depth ?? 1} / 上限 ${spec.max_depth ?? 3}`);
	if (spec.model?.worker) lines.push(`│ 指定模型: ${spec.model.worker}`);
	lines.push("└───────────────────────────────────────────");
	return lines.join("\n");
}

// ---------- the tool ----------

const fileTicketTool = defineTool({
	name: "file_ticket",
	label: "File Ticket",
	description:
		"入档一张正式 Ticket。这是任务进入系统的唯一入口。调用前必须已和用户确认草稿内容。" +
		"工具会渲染草稿并弹出人工确认，确认后才会写入系统。",
	promptSnippet: "把和用户确认好的任务草稿入档成正式 Ticket",
	promptGuidelines: [
		"使用 file_ticket 前，必须先和用户把 goal、acceptance、context 讨论清楚，绝不能凭空创建。",
		"file_ticket 会弹出人工确认框；用户拒绝时你必须回去继续沟通，不要重复调用。",
		"file_ticket 返回 ticket_id 后，告诉用户该 id，并提醒可以用 agent follow 查看进度。",
		"context 必须自包含：入档后 Router 会清空上下文，Worker 只能看到 ticket 里的内容。",
	],
	parameters: Type.Object({
		goal: Type.String({ description: "任务目标（>10 字，清晰可判定）" }),
		acceptance: Type.Array(Type.String(), {
			description: "验收标准，至少 1 条，每条可判定",
		}),
		context: Type.Optional(
			Type.Record(Type.String(), Type.Unknown(), {
				description: "自包含上下文：user_intent / known_facts / not_required 等",
			}),
		),
		max_depth: Type.Optional(Type.Number({ description: "允许的最大递归深度，默认 3，硬上限 3" })),
		worker_model: Type.Optional(Type.String({ description: "指定 worker 模型（成本控制，可选）" })),
		task_constraints: Type.Optional(
			Type.Array(Type.String(), { description: "单任务约束（L3），如『不许碰前端』" }),
		),
	}),

	async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
		// Build the spec
		const spec: TicketSpec = {
			goal: params.goal,
			acceptance: params.acceptance,
			context: params.context ?? {},
			depth: 1,
			max_depth: params.max_depth ?? 3,
		};
		if (params.worker_model) spec.model = { worker: params.worker_model };
		if (params.task_constraints && params.task_constraints.length > 0) {
			spec.constraints = { task_constraints: params.task_constraints };
		}

		// HUMAN GATE: render draft and require explicit confirmation.
		// This happens inside the tool, so the model cannot forge approval.
		if (ctx.hasUI) {
			const draft = renderDraft(spec);
			const ok = await ctx.ui.confirm("确认入档 Ticket", `${draft}\n\n确认后任务将进入系统并开始执行。`);
			if (!ok) {
				return {
					content: [
						{
							type: "text",
							text: "用户拒绝了这张 Ticket 草稿。请回去继续和用户沟通，确认清楚后再调用 file_ticket。",
						},
					],
					details: { filed: false, reason: "user_rejected" },
				};
			}
		}

		// Generate ticket id and admit via socket.
		// project = the Router's current working directory; it determines where
		// the Worker physically runs and which project rules apply. Immutable.
		const ticketId = `T-${Date.now().toString(36)}`;
		const wire = {
			spec: {
				id: ticketId,
				goal: spec.goal,
				acceptance: spec.acceptance,
				context: spec.context,
				depth: spec.depth,
				max_depth: spec.max_depth,
				session_id: ticketId,
				model: spec.model,
				constraints: spec.constraints,
				project: ctx.cwd,
			},
		};

		let resp: AgentdResponse;
		try {
			resp = await callAgentd("admit", wire);
		} catch (err) {
			return {
				content: [
					{
						type: "text",
						text: `无法连接 agentd：${String(err)}\n\n请让用户先运行 \`agent daemon\` 启动后台进程，然后再试。`,
					},
				],
				details: { filed: false, reason: "agentd_unreachable", error: String(err) },
			};
		}

		if (!resp.success) {
			return {
				content: [
					{
						type: "text",
						text: `agentd 拒绝了这张 Ticket：${resp.error ?? "unknown"}\n\n请修正 spec 后重试。`,
					},
				],
				details: { filed: false, reason: "agentd_rejected", error: resp.error },
			};
		}

		return {
			content: [
				{
					type: "text",
					text: `✅ Ticket 已入档：${ticketId}\n\n任务已进入系统，Worker 将开始执行。\n` +
						`用户可以用 \`agent follow ${ticketId}\` 或 \`agent show ${ticketId}\` 查看进度。\n` +
						`Router 现在应该 /new 清空上下文。`,
				},
			],
			details: { filed: true, ticket_id: ticketId },
		};
	},
});

// ---------- extension entry ----------

export default function (pi: ExtensionAPI) {
	// Only activate in `sa` (super-agent) sessions. Plain `pi` stays a Router-less
	// coding assistant with no file_ticket / create_subtask tools.
	// Marker: SA_MODE=1 exported by the `sa` wrapper (or `--sa` rewritten by shell).
	if (process.env.SA_MODE !== "1") {
		return;
	}

	pi.registerTool(fileTicketTool);

	// Also expose convenience commands for the human
	pi.registerCommand?.("tickets", {
		description: "List all tickets (via agentd)",
		handler: async (_args, ctx) => {
			try {
				const resp = await callAgentd("ls", {});
				if (!resp.success) {
					ctx.ui.notify(`agentd error: ${resp.error}`, "error");
					return;
				}
				const tickets = (resp.data as Array<Record<string, unknown>>) ?? [];
				if (tickets.length === 0) {
					ctx.ui.notify("没有 Ticket", "info");
					return;
				}
				const lines = tickets.map(
					(t) => `${String(t.id).padEnd(18)} ${String(t.state).padEnd(13)} ${String(t.goal).slice(0, 50)}`,
				);
				ctx.ui.notify(`Tickets:\n${lines.join("\n")}`, "info");
			} catch (err) {
				ctx.ui.notify(`无法连接 agentd: ${String(err)}`, "error");
			}
		},
	});
}
