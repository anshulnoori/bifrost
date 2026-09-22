import { afterEach, describe, expect, it, vi } from "vitest";
import { codexAction, codexConnectionSchema, codexModels, codexProbe } from "./codexApi";

afterEach(() => vi.unstubAllGlobals());

describe("Codex onboarding boundary", () => {
	it("discovers only admitted Codex models and tests through normal inference", async () => {
		const fetch = vi
			.fn()
			.mockResolvedValueOnce(new Response(JSON.stringify({ data: [{ id: "openai/other" }, { id: "codex/allowed" }] })))
			.mockResolvedValueOnce(
				new Response(
					JSON.stringify({
						status: "completed",
						output: [{ content: [{ type: "output_text", text: "OK" }] }],
						usage: { total_tokens: 37 },
					}),
				),
			);
		vi.stubGlobal("fetch", fetch);
		await expect(codexModels("key")).resolves.toEqual(["codex/allowed"]);
		await expect(codexProbe("key", "codex/allowed")).resolves.toEqual({ text: "OK", tokens: 37 });
		expect(fetch).toHaveBeenLastCalledWith(
			"/v1/responses",
			expect.objectContaining({
				headers: { "x-bf-vk": "key", "Content-Type": "application/json" },
				body: JSON.stringify({ model: "codex/allowed", input: "Reply with exactly OK.", stream: false, store: false }),
			}),
		);
	});
	it.each(["incomplete", "failed"])("does not call a %s inference result verified", async (status) => {
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ status, output: [] }))));
		await expect(codexProbe("key", "codex/allowed")).rejects.toThrow("did not complete");
	});
	it("does not reflect test-inference error bodies", async () => {
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("private-upstream-detail", { status: 429 })));
		await expect(codexProbe("key", "codex/allowed")).rejects.toEqual(
			new Error("The test request failed (HTTP 429). Check connection status, model access, and gateway limits."),
		);
	});
	it("recovers polling contention from the owner's current status", async () => {
		const fetch = vi
			.fn()
			.mockResolvedValueOnce(new Response("operation in progress", { status: 409 }))
			.mockResolvedValueOnce(new Response(JSON.stringify({ state: "polling", id: "own" })));
		vi.stubGlobal("fetch", fetch);
		await expect(codexAction("gateway-secret", "poll", "own")).resolves.toEqual({ state: "polling", id: "own" });
		expect(fetch).toHaveBeenLastCalledWith("/api/codex/connections/current", expect.objectContaining({ method: "GET" }));
	});
	it("sends gateway credentials only in a header and never caches the response", async () => {
		const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ state: "connected", id: "own" })));
		vi.stubGlobal("fetch", fetch);
		await expect(codexAction("gateway-secret", "poll", "own")).resolves.toMatchObject({
			state: "connected",
		});
		expect(fetch).toHaveBeenCalledWith(
			"/api/codex/connections/own/poll",
			expect.objectContaining({
				method: "POST",
				headers: { "x-bf-vk": "gateway-secret" },
				cache: "no-store",
			}),
		);
	});
	it("rejects an injected verification URL and strips credentials from the public shape", () => {
		expect(
			codexConnectionSchema.safeParse({
				state: "pending",
				verification_url: "https://attacker.invalid",
			}).success,
		).toBe(false);
		expect(
			codexConnectionSchema.parse({
				state: "connected",
				access_token: "secret",
				refresh_token: "secret",
			}),
		).toEqual({ state: "connected" });
	});
	it("does not reflect upstream errors or credentials", async () => {
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("upstream-secret", { status: 502 })));
		await expect(codexAction("gateway-secret", "start")).rejects.toThrow(
			"Codex authorization failed. Check status, then retry or reconnect.",
		);
	});
	it("forwards cancellation and uses DELETE to disconnect", async () => {
		const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ state: "disconnected" })));
		vi.stubGlobal("fetch", fetch);
		const controller = new AbortController();
		await codexAction("gateway-secret", "disconnect", "id/escaped", controller.signal);
		expect(fetch).toHaveBeenCalledWith(
			"/api/codex/connections/id%2Fescaped",
			expect.objectContaining({ method: "DELETE", signal: controller.signal }),
		);
	});
});