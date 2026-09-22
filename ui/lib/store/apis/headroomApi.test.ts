import { describe, expect, it } from "vitest";
import { headroomReportSchema } from "./headroomApi";

describe("Headroom report contract", () => {
	it("preserves unknown measurements instead of reporting zero savings", () => {
		const report = headroomReportSchema.parse({
			schema_version: 1,
			events: [],
			retention_seconds: 900,
			quality: "not_evaluated",
			ccr: "unsupported",
			cost_savings: null,
			attempt_coverage: "hooks",
		});
		expect(report.cost_savings).toBeNull();
		expect(report.quality).toBe("not_evaluated");
	});
	it("rejects a response that invents quality evidence", () => {
		expect(
			headroomReportSchema.safeParse({
				schema_version: 1,
				events: [],
				retention_seconds: 900,
				quality: "no_quality_loss",
				ccr: "unsupported",
				cost_savings: 0,
				attempt_coverage: "all",
			}).success,
		).toBe(false);
	});
});