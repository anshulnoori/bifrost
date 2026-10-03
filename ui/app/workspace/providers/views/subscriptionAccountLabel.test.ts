import { describe, expect, it } from "vitest";
import { subscriptionAccountAlias, subscriptionAccountLabel, type SubscriptionProvider } from "./subscriptionAccountLabel";
import { chatgptProviderKeyFieldsSchema, codexProviderKeyFieldsSchema, modelProviderKeyFieldsSchema } from "@/lib/types/schemas";

describe("subscription account labels", () => {
	it.each([
		["codex", "Personal", "person@example.test", "Personal (person@example.test)"],
		["codex", " Personal ", " person@example.test ", "Personal (person@example.test)"],
		["codex", "", "person@example.test", "person@example.test"],
		["codex", "PERSON@example.test", "person@example.test", "person@example.test"],
		["codex", "Codex deadbeef", "person@example.test", "person@example.test"],
		["chatgpt", "ChatGPT 01234567-1234-1234-1234-123456789abc", "person@example.test", "person@example.test"],
		["claude", "Claude deadbeef", "person@example.test", "person@example.test"],
		["codex", "Claude deadbeef", "person@example.test", "Claude deadbeef (person@example.test)"],
		["chatgpt", "ChatGPT Work", "person@example.test", "ChatGPT Work (person@example.test)"],
		["codex", "Personal", undefined, "Personal"],
	])("formats %s account %s with %s", (provider, name, email, expected) => {
		expect(subscriptionAccountLabel(name!, email, provider as SubscriptionProvider)).toBe(expected);
	});
	it("shows generated names as an empty editable alias", () => {
		expect(subscriptionAccountAlias("Codex deadbeef", "codex")).toBe("");
		expect(subscriptionAccountAlias("ChatGPT 01234567-1234-1234-1234-123456789abc", "chatgpt")).toBe("");
		expect(subscriptionAccountAlias("Claude deadbeef", "claude")).toBe("");
		expect(subscriptionAccountAlias("Claude deadbeef", "codex")).toBe("Claude deadbeef");
		expect(subscriptionAccountAlias(" My work ", "codex")).toBe("My work");
	});
	it("accepts blank aliases for subscription providers and keeps reserve validation", () => {
		const key = { id: "stable-id", name: "", weight: 1, codex_reserve_percent: 25 };
		expect(codexProviderKeyFieldsSchema.safeParse(key).success).toBe(true);
		expect(chatgptProviderKeyFieldsSchema.safeParse(key).success).toBe(true);
		expect(modelProviderKeyFieldsSchema.safeParse(key).success).toBe(false);
		expect(codexProviderKeyFieldsSchema.safeParse({ ...key, codex_reserve_percent: 101 }).success).toBe(false);
	});
});