import { z } from "zod";

export const CHATGPT_AUTHORIZATION_URL = "https://auth.openai.com/api/accounts/authorize" as const;
export const CHATGPT_CALLBACK_ORIGIN = "http://127.0.0.1:1455" as const;
export const CHATGPT_CALLBACK_PATH = "/auth/callback" as const;

const authorizationUrlSchema = z
	.string()
	.url()
	.refine((value) => {
		const url = new URL(value);
		return (
			url.origin === "https://auth.openai.com" && url.pathname === "/api/accounts/authorize" && !url.username && !url.password && !url.hash
		);
	}, "Invalid ChatGPT authorization URL.");

export const chatgptConnectionSchema = z.object({
	id: z.string().min(1).optional(),
	state: z.enum(["disconnected", "pending", "exchanging", "connected", "refreshing", "revoking", "expired", "reconnect_required"]),
	email: z.string().email().optional(),
	expires_at: z.string().datetime({ offset: true }).optional(),
	authorization_url: authorizationUrlSchema.optional(),
});
export type ChatGPTConnection = z.infer<typeof chatgptConnectionSchema>;

const callbackUrlSchema = z
	.string()
	.url()
	.superRefine((value, ctx) => {
		const url = new URL(value);
		if (url.origin !== CHATGPT_CALLBACK_ORIGIN || url.pathname !== CHATGPT_CALLBACK_PATH || url.username || url.password || url.hash) {
			ctx.addIssue({ code: "custom", message: `Paste the redirected ${CHATGPT_CALLBACK_ORIGIN}${CHATGPT_CALLBACK_PATH} URL.` });
		}
	});

export function parseChatGPTCallbackUrl(value: string): string {
	return callbackUrlSchema.parse(value.trim());
}

type ChatGPTAction = "start" | "status" | "complete" | "disconnect";

export async function chatgptAction(
	key: string,
	action: ChatGPTAction,
	id?: string,
	callbackUrl?: string,
	signal?: AbortSignal,
): Promise<ChatGPTConnection> {
	if ((action === "complete" || action === "disconnect") && !id) throw new Error("A ChatGPT connection ID is required.");

	const path = action === "start" ? "" : action === "status" ? "/current" : `/${encodeURIComponent(id!)}`;
	const method = action === "status" ? "GET" : action === "disconnect" ? "DELETE" : "POST";
	const body = action === "complete" ? JSON.stringify({ callback_url: parseChatGPTCallbackUrl(callbackUrl ?? "") }) : undefined;
	const response = await fetch(`/api/chatgpt/connections${path}${action === "complete" ? "/complete" : ""}`, {
		method,
		headers: {
			"x-bf-chatgpt-key": key,
			...(body ? { "content-type": "application/json" } : {}),
		},
		credentials: "same-origin",
		cache: "no-store",
		body,
		signal,
	});
	if (!response.ok) {
		const messages: Record<number, string> = {
			400: "The callback URL is invalid or expired. Start sign-in again.",
			409: "This ChatGPT connection changed. Check its status and retry.",
			503: "Configure encrypted database storage before connecting ChatGPT.",
		};
		throw new Error(messages[response.status] ?? "ChatGPT authorization failed. Check status, then retry or reconnect.");
	}
	return chatgptConnectionSchema.parse(await response.json());
}