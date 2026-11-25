/**
 * super-agent-worker — Worker extension for super_personal_agent
 *
 * Gives a headless pi Worker the `create_subtask` tool so it can recursively
 * split work. The tool calls agentd over the unix socket; agentd enforces the
 * 3-level recursion hard limit and parent/depth rules. The worker never writes
 * the ledger itself.
 *
 * Context is passed via environment variables set by agentd when spawning:
 *   SUPER_TICKET_ID   — this worker's ticket id (becomes the subtask's parent)
 *   SUPER_TICKET_DEPTH— this worker's current depth
 *   SUPER_SOCK        — path to the agentd socket
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { connect } from "node:net";
import { homedir } from "node:os";
import { join } from "node:path";

interface AgentdResponse {
	success: boolean;
	error?: string;
	data?: { ticket_id?: string };
}

function sockPath(): string {
	return process.env.SUPER_SOCK ?? join(homedir(), ".agent", "sock");
}

async function callAgentd(command: string, args: Record<string, unknown>): Promise<AgentdResponse> {
	return new Promise((resolve, reject) => {
		const conn = connect(sockPath());
		const chunks: Buffer[] = [];
		let settled = false;
		const fail = (e: Error) => {
			if (!settled) {
				settled = true;
				conn.destroy();
				reject(e);
			}
		};
		conn.setTimeout(8000);
		conn.on("timeout", () => fail(new Error("agentd timeout")));
		conn.on("error", (e) => fail(new Error(`connect agentd: ${e.message}`)));
		conn.on("data", (d) => chunks.push(d));
		conn.on("end", () => {
			if (settled) return;
			settled = true;
			try {
				const line = Buffer.concat(chunks).toString("utf8").split("\n").find((l) => l.trim()) ?? "{}";
				resolve(JSON.parse(line) as AgentdResponse);
			} catch (e) {
				reject(new Error(`parse response: ${String(e)}`));
			}
		});
		conn.on("connect", () => {
			conn.write(JSON.stringify({ id: "w1", command, args }) + "\n", () => conn.end());
		});
	});
}

const createSubtaskTool = defineTool({
	name: "create_subtask",
	label: "Create Subtask",
	description:
		"把当前任务的一部分拆成一个独立的子任务，由另一个 Worker 并行执行。" +
		"只有当你判断任务太大、一个人做不完，或某部分可独立完成时才使用。" +
		"子任务会继承你的模型，并有自己的独立上下文。",
	promptGuidelines: [
		"使用 create_subtask 前，先判断这部分工作是否真的独立、可并行，不要为了拆而拆。",
		"create_subtask 的 goal 和 acceptance 必须自包含：子 Worker 看不到你当前的对话，只能看到子任务本身。",
		"create_subtask 有最大递归深度限制（3 层）。到达上限后系统会让你停下来请求人工介入，而不是继续拆。",
		"创建子任务后，你应该继续做自己负责的部分；子任务的结果会在它完成后由系统汇总。",
	],
	parameters: Type.Object({
		goal: Type.String({ description: "子任务目标（自包含，>10 字）" }),
		acceptance: Type.Array(Type.String(), { description: "子任务验收标准，至少 1 条" }),
		context: Type.Optional(
			Type.Record(Type.String(), Type.Unknown(), { description: "子任务自包含上下文" }),
		),
		worker_model: Type.Optional(Type.String({ description: "覆盖子 Worker 模型（默认继承父模型）" })),
	}),

	async execute(_toolCallId, params, _signal, _onUpdate, _ctx) {
		const parentId = process.env.SUPER_TICKET_ID;
		const depthStr = process.env.SUPER_TICKET_DEPTH ?? "1";
		const parentDepth = parseInt(depthStr, 10) || 1;
		const myDepth = parentDepth + 1;

		if (!parentId) {
			return {
				content: [{ type: "text", text: "错误：缺少 SUPER_TICKET_ID，无法确定父任务。请不要直接使用 create_subtask。" }],
				details: { created: false, reason: "no_parent_context" },
			};
		}

		// Hard client-side guard too (agentd is authoritative, but fail fast)
		if (myDepth > 3) {
			return {
				content: [
					{
						type: "text",
						text:
							"已达到最大递归深度（3 层），不能再拆分子任务。\n" +
							"这说明当前任务可能比预期复杂，或拆分方向有误。\n" +
							"请停止拆分，输出一行 `NEED_INPUT: 任务已到达最大递归深度仍未完成，需要人工判断是 AI 走偏还是需求理解有误` 让人工介入。",
					},
				],
				details: { created: false, reason: "max_depth", depth: myDepth },
			};
		}

		const ticketId = `T-sub-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4).toString(36)}`;
		const spec: Record<string, unknown> = {
			id: ticketId,
			parent_id: parentId,
			goal: params.goal,
			acceptance: params.acceptance,
			context: params.context ?? {},
			depth: myDepth,
			max_depth: 3,
			session_id: ticketId,
			// Subtask inherits the parent's project (same physical workspace root)
			project: process.env.SUPER_PROJECT ?? "",
		};
		if (params.worker_model) {
			spec.model = { worker: params.worker_model, subagent: "inherit" };
		}

		let resp: AgentdResponse;
		try {
			resp = await callAgentd("admit", { spec });
		} catch (err) {
			return {
				content: [{ type: "text", text: `无法连接 agentd 创建子任务：${String(err)}` }],
				details: { created: false, reason: "agentd_unreachable", error: String(err) },
			};
		}

		if (!resp.success) {
			// agentd rejected — possibly max depth. Surface clearly.
			return {
				content: [
					{
						type: "text",
						text: `agentd 拒绝了子任务：${resp.error ?? "unknown"}\n\n如果是因为达到最大递归深度，请停止拆分并输出 NEED_INPUT 让人工介入。`,
					},
				],
				details: { created: false, reason: "agentd_rejected", error: resp.error },
			};
		}

		return {
			content: [
				{
					type: "text",
					text: `✅ 已创建子任务 ${resp.data?.ticket_id}（第 ${myDepth} 层），将由另一个 Worker 并行执行。\n请继续完成你自己负责的部分。`,
				},
			],
			details: { created: true, ticket_id: resp.data?.ticket_id, depth: myDepth },
		};
	},
});

export default function (pi: ExtensionAPI) {
	// Only register when running as a spawned worker (env marker present).
	// This keeps interactive Router sessions clean.
	if (process.env.SUPER_TICKET_ID) {
		pi.registerTool(createSubtaskTool);
	}
}
