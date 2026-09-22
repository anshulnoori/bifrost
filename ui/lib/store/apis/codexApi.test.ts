import { afterEach, describe, expect, it, vi } from "vitest";
import { codexAction, codexConnectionSchema } from "./codexApi";

afterEach(() => vi.unstubAllGlobals());

describe("Codex onboarding boundary", () => {
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