import { describe, expect, it } from "vitest";
import { codexAccountAlias, codexAccountLabel } from "./codexAccountLabel";
import { codexProviderKeyFieldsSchema, modelProviderKeyFieldsSchema } from "@/lib/types/schemas";

describe("Codex account labels", () => {
	it.each([
		["Personal", "person@example.test", "Personal (person@example.test)"],
		[" Personal ", " person@example.test ", "Personal (person@example.test)"],
		["", "person@example.test", "person@example.test"],
		["   ", "person@example.test", "person@example.test"],
		["PERSON@example.test", "person@example.test", "person@example.test"],
		["Codex deadbeef", "person@example.test", "person@example.test"],
		["Codex 01234567-1234-1234-1234-123456789abc", "person@example.test", "person@example.test"],
		["Codex Work", "person@example.test", "Codex Work (person@example.test)"],
		["Personal", undefined, "Personal"],
		["Codex deadbeef", undefined, "Codex deadbeef"],
	])("formats %s with %s", (name, email, expected) => {
		expect(codexAccountLabel(name!, email)).toBe(expected);
	});
	it("shows generated names as an empty editable alias", () => {
		expect(codexAccountAlias("Codex deadbeef")).toBe("");
		expect(codexAccountAlias("Codex 01234567-1234-1234-1234-123456789abc")).toBe("");
		expect(codexAccountAlias(" My work ")).toBe("My work");
	});
	it("accepts blank aliases only for Codex and keeps reserve validation", () => {
		const key = { id: "stable-id", name: "", weight: 1, codex_reserve_percent: 25 };
		expect(codexProviderKeyFieldsSchema.safeParse(key).success).toBe(true);
		expect(modelProviderKeyFieldsSchema.safeParse(key).success).toBe(false);
		expect(codexProviderKeyFieldsSchema.safeParse({ ...key, codex_reserve_percent: 101 }).success).toBe(false);
	});
});