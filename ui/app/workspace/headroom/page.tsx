import { useEffect, useState } from "react";
import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from "recharts";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { HeadroomReport, readHeadroomReport } from "@/lib/store/apis/headroomApi";
import HeadroomConfiguration from "./configuration";

export default function HeadroomPage() {
	const [token, setToken] = useState("");
	const [report, setReport] = useState<HeadroomReport | null>(null);
	const [filter, setFilter] = useState("");
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(false);
	const events =
		report?.events.filter((event) =>
			[event.provider, event.model, event.project, event.principal_hash, event.thread_hash].some((value) =>
				value.toLowerCase().includes(filter.toLowerCase()),
			),
		) ?? [];
	const estimated = events.filter((event) => event.tool_result_estimate !== null);
	const before = estimated.reduce((sum, event) => sum + event.tool_result_estimate!.before_estimated_tokens, 0);
	const after = estimated.reduce((sum, event) => sum + event.tool_result_estimate!.after_estimated_tokens, 0);
	const trend = [...estimated]
		.sort((a, b) => Date.parse(a.started) - Date.parse(b.started))
		.map((event) => ({
			time: new Date(event.started).toLocaleTimeString(),
			before: event.tool_result_estimate!.before_estimated_tokens,
			after: event.tool_result_estimate!.after_estimated_tokens,
		}));
	useEffect(() => {
		void refresh();
	}, []);
	async function refresh() {
		setLoading(true);
		setError("");
		setReport(null);
		try {
			setReport(await readHeadroomReport(token));
		} catch (e) {
			setError(e instanceof Error ? e.message : "Unable to load monitoring data.");
		} finally {
			setLoading(false);
		}
	}
	return (
		<div className="mx-auto flex w-full max-w-7xl flex-col gap-6" data-testid="headroom-page">
			<div className="flex flex-wrap items-start justify-between gap-4">
				<div>
					<h1 className="text-2xl font-semibold">Headroom</h1>
					<p className="text-muted-foreground mt-1 text-sm">Tool-result compression and usage</p>
				</div>
			</div>
			<Tabs defaultValue="overview">
				<TabsList>
					<TabsTrigger value="overview" data-testid="headroom-overview-tab">
						Overview
					</TabsTrigger>
					<TabsTrigger value="configuration" data-testid="headroom-configuration-tab">
						Configuration
					</TabsTrigger>
				</TabsList>
				<TabsContent value="configuration" className="mt-4">
					<HeadroomConfiguration />
				</TabsContent>
				<TabsContent value="overview" className="mt-4 flex flex-col gap-6">
					<form
						className="flex max-w-2xl flex-wrap items-end gap-3"
						onSubmit={(e) => {
							e.preventDefault();
							void refresh();
						}}
					>
						{!report && (
							<div className="min-w-64 flex-1">
								<label htmlFor="headroom-token" className="mb-2 block text-sm">
									Monitoring token (if dashboard login is unavailable)
								</label>
								<Input
									id="headroom-token"
									type="password"
									autoComplete="off"
									value={token}
									onChange={(e) => setToken(e.target.value)}
									data-testid="headroom-token"
								/>
							</div>
						)}
						<Button type="submit" disabled={loading} data-testid="headroom-refresh">
							{loading ? "Loading…" : "Refresh"}
						</Button>
						<Button
							type="button"
							variant="outline"
							disabled={loading}
							onClick={() => {
								setToken("");
								setReport(null);
								setError("");
							}}
							data-testid="headroom-clear"
						>
							Clear
						</Button>
					</form>
					<p className="text-muted-foreground -mt-3 text-xs">
						Metrics load using your dashboard session. Refresh to include new requests. Any monitoring token stays in this page’s memory.
					</p>
					{error && (
						<p role="alert" className="text-destructive" data-testid="headroom-error">
							{error}
						</p>
					)}
					{!report ? (
						<div className="text-muted-foreground rounded-md border border-dashed p-10 text-center" data-testid="headroom-empty">
							Load a snapshot to view compression and usage.
						</div>
					) : (
						<>
							<Input
								aria-label="Filter Headroom events"
								placeholder="Filter provider, model, project, principal or thread hash"
								value={filter}
								onChange={(e) => setFilter(e.target.value)}
								data-testid="headroom-filter"
							/>
							<div className="grid gap-4 md:grid-cols-4">
								{["compressed", "bypassed", "failed"].map((status) => (
									<Card key={status}>
										<CardHeader>
											<CardTitle className="text-sm capitalize">{status}</CardTitle>
										</CardHeader>
										<CardContent className="text-3xl font-semibold">{events.filter((event) => event.status === status).length}</CardContent>
									</Card>
								))}
								<Card>
									<CardHeader>
										<CardTitle className="text-sm">Eligible attempts</CardTitle>
									</CardHeader>
									<CardContent className="text-3xl font-semibold">{events.filter((event) => event.eligible).length}</CardContent>
								</Card>
							</div>
							<div className="grid gap-4 md:grid-cols-3">
								<Card>
									<CardHeader>
										<CardTitle className="text-sm">Estimated tool-result tokens</CardTitle>
									</CardHeader>
									<CardContent>
										<div className="text-2xl font-semibold">
											{estimated.length ? `${before.toLocaleString()} → ${after.toLocaleString()}` : "Unknown"}
										</div>
										<p className="text-muted-foreground mt-2 text-xs">Tool-result estimates across {estimated.length} attempts.</p>
										<p className="mt-2 text-sm" data-testid="headroom-token-savings">
											{before > 0
												? `${(before - after).toLocaleString()} fewer tokens (${(((before - after) / before) * 100).toFixed(1)}%)`
												: "No token estimates yet"}
										</p>
									</CardContent>
								</Card>
								<Card>
									<CardHeader>
										<CardTitle className="text-sm">Actual cost / modeled savings</CardTitle>
									</CardHeader>
									<CardContent>
										Not measured
										<p className="text-muted-foreground mt-2 text-xs">View provider costs in Bifrost billing logs.</p>
									</CardContent>
								</Card>
								<Card>
									<CardHeader>
										<CardTitle className="text-sm">Evaluation</CardTitle>
									</CardHeader>
									<CardContent>No results yet</CardContent>
								</Card>
							</div>
							<Card>
								<CardHeader>
									<CardTitle className="text-sm">Tool-result tokens per attempt</CardTitle>
								</CardHeader>
								<CardContent>
									{trend.length ? (
										<div className="h-64" data-testid="headroom-token-chart">
											<ResponsiveContainer width="100%" height="100%">
												<LineChart data={trend} margin={{ left: 12, right: 16, top: 8, bottom: 8 }}>
													<CartesianGrid strokeDasharray="3 3" vertical={false} />
													<XAxis dataKey="time" minTickGap={32} tick={{ fontSize: 12 }} />
													<YAxis tick={{ fontSize: 12 }} />
													<Tooltip />
													<Legend />
													<Line
														dataKey="before"
														name="Before (estimated)"
														stroke="var(--chart-1)"
														strokeWidth={2}
														isAnimationActive={false}
													/>
													<Line
														dataKey="after"
														name="After (estimated)"
														stroke="var(--chart-2)"
														strokeWidth={2}
														isAnimationActive={false}
													/>
												</LineChart>
											</ResponsiveContainer>
										</div>
									) : (
										<p className="text-muted-foreground py-12 text-center">Send eligible tool-result requests to see compression trends.</p>
									)}
									<p className="text-muted-foreground mt-3 text-xs">
										Estimated tool-result tokens only, not total prompt tokens or billed savings. Each point is one retained attempt.
									</p>
								</CardContent>
							</Card>
							<p className="text-muted-foreground text-xs">
								Up to 1,000 events on this replica · retention {report.retention_seconds}s · {events.length} matching events.
							</p>
							<div className="overflow-x-auto rounded-md border">
								<Table>
									<TableHeader>
										<TableRow>
											{["Provider / model", "Project / thread", "Outcome", "Compression / attempt", "Provider-reported usage"].map(
												(title) => (
													<TableHead key={title}>{title}</TableHead>
												),
											)}
										</TableRow>
									</TableHeader>
									<TableBody>
										{events.map((event) => (
											<TableRow key={`${event.started}-${event.thread_hash}-${event.provider}-${event.model}`}>
												<TableCell>
													{event.provider}
													<div className="text-muted-foreground">{event.model}</div>
												</TableCell>
												<TableCell>
													{event.project || "Unknown"}
													<div className="font-mono text-xs">{event.thread_hash.slice(0, 12) || "Unknown"}</div>
												</TableCell>
												<TableCell>
													{event.status}
													<div className="text-muted-foreground text-xs">{event.reason}</div>
												</TableCell>
												<TableCell>
													{event.compression_ms.toFixed(1)} / {event.plugin_attempt_ms.toFixed(1)} ms
													<div className="text-muted-foreground text-xs">{event.network_retries} retries; intermediate usage unknown</div>
												</TableCell>
												<TableCell className="max-w-80">
													<pre className="text-xs break-all whitespace-pre-wrap">
														{event.provider_usage ? JSON.stringify(event.provider_usage, null, 2) : "Unknown (not reported)"}
													</pre>
												</TableCell>
											</TableRow>
										))}
									</TableBody>
								</Table>
								{events.length === 0 && <p className="text-muted-foreground p-6">No retained events match this filter.</p>}
							</div>
						</>
					)}
				</TabsContent>
			</Tabs>
		</div>
	);
}