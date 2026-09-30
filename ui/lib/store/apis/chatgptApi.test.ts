import { afterEach, describe, expect, it, vi } from "vitest";
import { CHATGPT_AUTHORIZATION_URL, chatgptAction, chatgptConnectionSchema, parseChatGPTCallbackUrl } from "./chatgptApi";

afterEach(() => vi.unstubAllGlobals());

describe("ChatGPT authorization boundary", () => {
	it("accepts only the fixed authorization URL and documented states", () => {
		expect(chatgptConnectionSchema.safeParse({ state: "pending", authorization_url: CHATGPT_AUTHORIZATION_URL }).success).toBe(true);
		expect(
			chatgptConnectionSchema.safeParse({
				state: "pending",
				authorization_url: `${CHATGPT_AUTHORIZATION_URL}?state=opaque&client_id=dynamic_agent_client`,
			}).success,
		).toBe(true);
		expect(chatgptConnectionSchema.safeParse({ state: "polling", authorization_url: CHATGPT_AUTHORIZATION_URL }).success).toBe(false);
		expect(chatgptConnectionSchema.safeParse({ state: "pending", authorization_url: "https://attacker.invalid" }).success).toBe(false);
		expect(
			chatgptConnectionSchema.safeParse({
				state: "pending",
				authorization_url: "https://auth.openai.com@attacker.invalid/api/accounts/authorize",
			}).success,
		).toBe(false);
		expect(
			chatgptConnectionSchema.safeParse({ state: "pending", authorization_url: "https://auth.openai.com/api/accounts/other" }).success,
		).toBe(false);
	});

	it("accepts only the loopback callback origin and path", () => {
		expect(parseChatGPTCallbackUrl(" http://127.0.0.1:1455/auth/callback?code=secret&state=value ")).toContain("code=secret");
		expect(() => parseChatGPTCallbackUrl("http://localhost:1455/auth/callback?code=x")).toThrow();
		expect(() => parseChatGPTCallbackUrl("http://127.0.0.1:1455/other?code=x")).toThrow();
		expect(() => parseChatGPTCallbackUrl("https://127.0.0.1:1455/auth/callback?code=x")).toThrow();
	});

	it("posts a validated callback without exposing it in the URL", async () => {
		const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "connection", state: "connected" })));
		vi.stubGlobal("fetch", fetch);
		const callback = "http://127.0.0.1:1455/auth/callback?code=private&state=private";
		await chatgptAction("provider-key-id", "complete", "connection/id", callback);
		expect(fetch).toHaveBeenCalledWith(
			"/api/chatgpt/connections/connection%2Fid/complete",
			expect.objectContaining({
				method: "POST",
				headers: { "x-bf-chatgpt-key": "provider-key-id", "content-type": "application/json" },
				credentials: "same-origin",
				body: JSON.stringify({ callback_url: callback }),
			}),
		);
	});

	it("uses current status and DELETE routes without returning upstream details", async () => {
		const fetch = vi
			.fn()
			.mockResolvedValueOnce(new Response(JSON.stringify({ state: "disconnected" })))
			.mockResolvedValueOnce(new Response("private", { status: 502 }));
		vi.stubGlobal("fetch", fetch);
		await chatgptAction("key", "status");
		expect(fetch).toHaveBeenLastCalledWith("/api/chatgpt/connections/current", expect.objectContaining({ method: "GET" }));
		await expect(chatgptAction("key", "disconnect", "connection")).rejects.toThrow("ChatGPT authorization failed");
		expect(fetch).toHaveBeenLastCalledWith("/api/chatgpt/connections/connection", expect.objectContaining({ method: "DELETE" }));
	});
});