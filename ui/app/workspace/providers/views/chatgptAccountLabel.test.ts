import { describe, expect, it } from "vitest";
import { chatgptAccountAlias, chatgptAccountLabel } from "./chatgptAccountLabel";
import { chatgptProviderKeyFieldsSchema, modelProviderKeyFieldsSchema } from "@/lib/types/schemas";

describe("ChatGPT account labels", () => {
	it.each([
		["Personal", "person@example.test", "Personal (person@example.test)"],
		[" Personal ", " person@example.test ", "Personal (person@example.test)"],
		["", "person@example.test", "person@example.test"],
		["   ", "person@example.test", "person@example.test"],
		["PERSON@example.test", "person@example.test", "person@example.test"],
		["ChatGPT deadbeef", "person@example.test", "person@example.test"],
		["ChatGPT 01234567-1234-1234-1234-123456789abc", "person@example.test", "person@example.test"],
		["ChatGPT Work", "person@example.test", "ChatGPT Work (person@example.test)"],
		["Personal", undefined, "Personal"],
		["ChatGPT deadbeef", undefined, "ChatGPT deadbeef"],
	])("formats %s with %s", (name, email, expected) => {
		expect(chatgptAccountLabel(name!, email)).toBe(expected);
	});
	it("shows generated names as an empty editable alias", () => {
		expect(chatgptAccountAlias("ChatGPT deadbeef")).toBe("");
		expect(chatgptAccountAlias("ChatGPT 01234567-1234-1234-1234-123456789abc")).toBe("");
		expect(chatgptAccountAlias(" My work ")).toBe("My work");
	});
	it("accepts blank aliases only for ChatGPT", () => {
		const key = { id: "stable-id", name: "", weight: 1 };
		expect(chatgptProviderKeyFieldsSchema.safeParse(key).success).toBe(true);
		expect(modelProviderKeyFieldsSchema.safeParse(key).success).toBe(false);
	});
});