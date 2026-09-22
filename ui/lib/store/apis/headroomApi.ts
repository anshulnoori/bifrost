import { z } from "zod";

const eventSchema = z.object({
	started: z.string(),
	provider: z.string(),
	model: z.string(),
	project: z.string(),
	principal_hash: z.string(),
	thread_hash: z.string(),
	status: z.enum(["compressed", "bypassed", "failed"]),
	reason: z.string(),
	eligible: z.boolean(),
	quality: z.literal("not_evaluated"),
	tool_result_estimate: z
		.object({ before_estimated_tokens: z.number().nonnegative(), after_estimated_tokens: z.number().nonnegative() })
		.nullable(),
	compression_ms: z.number().nonnegative(),
	plugin_attempt_ms: z.number().nonnegative(),
	provider_usage: z.record(z.string(), z.unknown()).nullable(),
	network_retries: z.number().int().nonnegative(),
});
export const headroomReportSchema = z.object({
	schema_version: z.literal(1),
	events: z.array(eventSchema).max(1000),
	retention_seconds: z.number(),
	quality: z.literal("not_evaluated"),
	ccr: z.literal("unsupported"),
	cost_savings: z.null(),
	attempt_coverage: z.string(),
});
export type HeadroomEvent = z.infer<typeof eventSchema>;
export type HeadroomReport = z.infer<typeof headroomReportSchema>;

// Do not put this credential in RTK Query arguments, Redux, a URL, or storage.
// The snapshot is deliberately fetched with component-local state only.
export async function readHeadroomReport(token: string): Promise<HeadroomReport> {
	const response = await fetch("/api/headroom/events", {
		credentials: "same-origin",
		cache: "no-store",
		signal: AbortSignal.timeout(5000),
		headers: { "X-Headroom-Admin-Token": token },
	});
	if (!response.ok)
		throw new Error(
			response.status === 401
				? "Sign in as a dashboard administrator or enter a valid monitoring token."
				: "Monitoring is unavailable. Check the plugin listener and gateway registration.",
		);
	return headroomReportSchema.parse(await response.json());
}
