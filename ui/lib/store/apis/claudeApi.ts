import { z } from "zod";

const authorizationURL = z
	.string()
	.url()
	.refine((value) => {
		const url = new URL(value);
		return url.origin === "https://claude.com" && url.pathname === "/cai/oauth/authorize" && !url.username && !url.password && !url.hash;
	});
const connectionSchema = z.object({
	id: z.string().optional(),
	state: z.enum(["disconnected", "pending", "connecting", "connected", "expired", "reconnect_required"]),
	email: z.string().optional(),
	authorization_url: authorizationURL.optional(),
	interval_seconds: z.number().int().min(1).max(60).optional(),
});
export type ClaudeConnection = z.infer<typeof connectionSchema>;

const usageWindow = z.object({ utilization: z.number(), resets_at: z.string().optional() });
const usageSchema = z.object({
	five_hour: usageWindow.optional(),
	seven_day: usageWindow.optional(),
	seven_day_opus: usageWindow.optional(),
	seven_day_sonnet: usageWindow.optional(),
	models: z.array(usageWindow.extend({ name: z.string() })).optional(),
});
export type ClaudeUsage = z.infer<typeof usageSchema>;

export async function claudeUsage(key: string, signal?: AbortSignal): Promise<ClaudeUsage> {
	const response = await fetch("/api/claude/connections/usage", {
		headers: { "x-bf-claude-key": key },
		credentials: "same-origin",
		cache: "no-store",
		signal,
	});
	if (!response.ok) throw new Error("Usage unavailable");
	return usageSchema.parse(await response.json());
}

export async function claudeAction(
	key: string,
	action: "status" | "start" | "code" | "disconnect",
	signal?: AbortSignal,
	code?: { id: string; code: string },
): Promise<ClaudeConnection> {
	const path = action === "start" ? "" : action === "code" ? "/code" : "/current";
	const response = await fetch(`/api/claude/connections${path}`, {
		method: action === "status" ? "GET" : action === "disconnect" ? "DELETE" : "POST",
		headers: { "x-bf-claude-key": key, "content-type": "application/json" },
		credentials: "same-origin",
		cache: "no-store",
		signal,
		...(code ? { body: JSON.stringify(code) } : {}),
	});
	if (!response.ok) {
		const messages: Record<number, string> = {
			400: "The authorization code or state is invalid. Copy the complete code#state value from Claude.",
			401: "Sign in to the dashboard first.",
			403: "Dashboard account-management access is required.",
			404: "Claude account not found. Save the account first.",
			409: "The login is no longer pending. Reconnect to Claude.",
		};
		throw new Error(messages[response.status] ?? "Claude account operation failed. Retry or reconnect.");
	}
	return connectionSchema.parse(await response.json());
}
