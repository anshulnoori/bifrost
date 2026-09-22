import { z } from "zod";

export const codexConnectionSchema = z.object({
	id: z.string().optional(),
	state: z.enum(["disconnected", "pending", "polling", "connected", "refreshing", "expired", "reconnect_required"]),
	expires_at: z.string().optional(),
	verification_url: z.literal("https://auth.openai.com/codex/device").optional(),
	user_code: z.string().optional(),
	interval_seconds: z.number().int().min(1).max(60).optional(),
});
export type CodexConnection = z.infer<typeof codexConnectionSchema>;

// Like Headroom's explicit admin credential, keep the virtual key out of Redux
// action history, RTK query arguments, URLs, and persistent browser storage.
export async function codexAction(
	key: string,
	action: "status" | "start" | "poll" | "disconnect",
	id?: string,
	signal?: AbortSignal,
): Promise<CodexConnection> {
	const path =
		action === "status" ? "/current" : action === "start" ? "" : `/${encodeURIComponent(id ?? "")}${action === "poll" ? "/poll" : ""}`;
	const response = await fetch(`/api/codex/connections${path}`, {
		method: action === "status" ? "GET" : action === "disconnect" ? "DELETE" : "POST",
		headers: { "x-bf-vk": key },
		credentials: "same-origin",
		cache: "no-store",
		signal,
	});
	// Another replica may own the exchange, or a failed exchange may already
	// have moved to reconnect_required. Read the current state in either case.
	if (action === "poll" && response.status === 409) {
		return codexAction(key, "status", undefined, signal);
	}
	if (!response.ok) {
		const messages: Record<number, string> = {
			401: "Enter an active Bifrost virtual key that allows Codex.",
			403: "This gateway identity cannot manage the Codex connection.",
			404: "This connection no longer exists. Check status or reconnect.",
			409: "An operation is in progress or reconnect is required. Check status.",
			503: "Configure encrypted database storage before connecting Codex.",
		};
		throw new Error(messages[response.status] ?? "Codex authorization failed. Check status, then retry or reconnect.");
	}
	return codexConnectionSchema.parse(await response.json());
}

export async function codexModels(key: string, signal?: AbortSignal): Promise<string[]> {
	const response = await fetch("/v1/models?provider=codex", {
		headers: { "x-bf-vk": key },
		credentials: "same-origin",
		cache: "no-store",
		signal,
	});
	if (!response.ok) throw new Error("Could not load your models. Check the connection and virtual-key permissions, then retry.");
	const data = z.object({ data: z.array(z.object({ id: z.string() })) }).safeParse(await response.json());
	if (!data.success) throw new Error("The gateway returned an invalid model catalog.");
	return data.data.data.map((model) => model.id).filter((id) => id.startsWith("codex/"));
}

export async function codexProbe(key: string, model: string, signal?: AbortSignal): Promise<{ text: string; tokens?: number }> {
	const response = await fetch("/v1/responses", {
		method: "POST",
		headers: { "x-bf-vk": key, "Content-Type": "application/json" },
		credentials: "same-origin",
		cache: "no-store",
		signal,
		body: JSON.stringify({ model, input: "Reply with exactly OK.", stream: false, store: false }),
	});
	if (!response.ok)
		throw new Error(`The test request failed (HTTP ${response.status}). Check connection status, model access, and gateway limits.`);
	const result = z
		.object({
			status: z.string(),
			output: z.array(z.object({ content: z.array(z.object({ type: z.string(), text: z.string().optional() })).optional() })),
			usage: z.object({ total_tokens: z.number() }).optional(),
		})
		.safeParse(await response.json());
	if (!result.success || result.data.status !== "completed")
		throw new Error("The test request did not complete. Check the gateway logs, then retry.");
	const text = result.data.output
		.flatMap((item) => item.content ?? [])
		.filter((item) => item.type === "output_text")
		.map((item) => item.text ?? "")
		.join("");
	if (!text) throw new Error("The test completed without a text response. Try another available model.");
	return { text: text.slice(0, 500), tokens: result.data.usage?.total_tokens };
}