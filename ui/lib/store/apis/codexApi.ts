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