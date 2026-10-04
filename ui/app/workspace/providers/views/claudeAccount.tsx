import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Progress } from "@/components/ui/progress";
import { TableCell, TableRow } from "@/components/ui/table";
import { claudeAction, claudeUsage, type ClaudeConnection as Connection, type ClaudeUsage } from "@/lib/store/apis/claudeApi";
import { ModelProviderKey } from "@/lib/types/config";
import { useVisiblePolling } from "@/hooks/useVisiblePolling";
import { formatDistanceToNow } from "date-fns";
import { ChevronDown, ExternalLink, RefreshCw } from "lucide-react";
import { ReactNode, useEffect, useState } from "react";
import { subscriptionAccountLabel } from "./subscriptionAccountLabel";

function usageWindows(usage?: ClaudeUsage) {
	if (!usage) return [];
	return [
		{ id: "five_hour", label: "5h session", window: usage.five_hour, headline: true },
		{ id: "seven_day", label: "7d all models", window: usage.seven_day, headline: true },
		{ id: "seven_day_opus", label: "7d Opus", window: usage.seven_day_opus, headline: false },
		{ id: "seven_day_sonnet", label: "7d Sonnet", window: usage.seven_day_sonnet, headline: false },
		...(usage.models ?? []).map((model) => ({ id: `model:${model.name}`, label: `7d ${model.name}`, window: model, headline: false })),
	].flatMap(({ window, ...rest }) =>
		window ? [{ ...rest, remaining: Math.max(0, Math.min(100, 100 - window.utilization)), resetsAt: window.resets_at }] : [],
	);
}

export default function ClaudeAccount({
	account,
	revision,
	canUpdate,
	onEdit,
	menu,
	onLoading,
}: {
	account: ModelProviderKey;
	revision: number;
	canUpdate: boolean;
	onEdit: () => void;
	menu: ReactNode;
	onLoading?: (id: string, loading: boolean) => void;
}) {
	const [open, setOpen] = useState(false);
	const [connection, setConnection] = useState<Connection>();
	const [usage, setUsage] = useState<ClaudeUsage>();
	const [usageError, setUsageError] = useState(false);
	const [error, setError] = useState("");
	const [refresh, setRefresh] = useState(0);
	const loading = useVisiblePolling(
		async (signal) => {
			try {
				const result = await claudeAction(account.id, "status", signal);
				if (signal.aborted) return;
				setConnection(result);
				setError("");
				if (result.state !== "connected") {
					setUsage(undefined);
					setUsageError(false);
					return;
				}
				try {
					const value = await claudeUsage(account.id, signal);
					if (!signal.aborted) {
						setUsage(value);
						setUsageError(false);
					}
				} catch {
					// Never show a stale or fabricated allowance when the read fails.
					if (!signal.aborted) {
						setUsage(undefined);
						setUsageError(true);
					}
				}
			} catch {
				if (!signal.aborted) setError("Status unavailable");
			}
		},
		[account.id, revision, refresh],
	);
	useEffect(() => {
		onLoading?.(account.id, loading);
	}, [account.id, loading, onLoading]);
	useEffect(() => () => onLoading?.(account.id, false), [account.id, onLoading]);
	const label = subscriptionAccountLabel(account.name, connection?.email, "claude");
	const checkedAt = usage?.checked_at ? new Date(usage.checked_at) : undefined;
	const status = account.enabled === false ? "Inactive" : error || connection?.state.replaceAll("_", " ") || "Loading…";
	const windows = usageWindows(usage);
	// The tighter of the session and weekly windows determines what is usable now.
	const headlineWindows = windows.filter((window) => window.headline);
	const headline = headlineWindows.length ? Math.min(...headlineWindows.map((window) => window.remaining)) : undefined;
	// Mirrors the gateway rule: only the weekly all-models window governs the reserve.
	const reserve = account.codex_reserve_percent;
	const weekly = usage?.seven_day?.utilization;
	const reserveStatus =
		reserve == null || connection?.state !== "connected"
			? undefined
			: weekly == null || weekly < 0 || weekly > 100
				? "Usage unavailable"
				: 100 - weekly <= reserve
					? "Reserve reached"
					: undefined;
	return (
		<TableRow data-testid={`key-row-${account.name}`} className="hover:bg-transparent">
			<TableCell colSpan={4} className="p-0 whitespace-normal">
				<Collapsible open={open} onOpenChange={setOpen} data-testid={`claude-account-${account.id}`}>
					<CollapsibleTrigger asChild>
						<button
							type="button"
							className="hover:bg-muted/50 relative flex w-full flex-wrap items-center gap-3 px-4 py-4 pr-10 text-left sm:px-5"
							aria-label={`${label} subscription details`}
						>
							<span className="min-w-0 basis-full font-medium [overflow-wrap:anywhere] sm:flex-1">{label}</span>
							<span className="text-muted-foreground text-xs">Claude subscription</span>
							{headline !== undefined && (
								<span className="flex items-center gap-2 text-xs tabular-nums">
									<Progress className="h-1.5 w-20" value={headline} aria-label="Remaining allowance" aria-valuenow={headline} />
									<span>{Math.round(headline)}%</span>
								</span>
							)}
							<Badge variant="secondary" className="capitalize">
								{account.enabled === false ? status : (reserveStatus ?? status)}
							</Badge>
							<ChevronDown
								className={`absolute top-5 right-4 size-4 shrink-0 transition-transform sm:static ${open ? "rotate-180" : ""}`}
							/>
						</button>
					</CollapsibleTrigger>
					<CollapsibleContent className="border-t px-4 py-4 sm:px-5">
						<dl className="grid grid-cols-1 items-start gap-x-6 gap-y-2 text-sm [overflow-wrap:anywhere] sm:grid-cols-[auto_minmax(0,1fr)] sm:gap-y-4 [&>dt:not(:first-child)]:mt-2 sm:[&>dt:not(:first-child)]:mt-0">
							<dt className="text-muted-foreground">Account</dt>
							<dd data-testid="claude-account-email">{connection?.email || "Email unavailable"}</dd>
							<dt className="text-muted-foreground">Models</dt>
							<dd>
								{!account.models?.length || account.models.includes("*") ? "All available Claude models" : account.models.join(", ")}
								{!!account.blacklisted_models?.length && (
									<span className="text-muted-foreground"> · Excluded: {account.blacklisted_models.join(", ")}</span>
								)}
							</dd>
							<dt className="text-muted-foreground">Billing</dt>
							<dd>Uses Claude subscription allowance</dd>
							<dt className="text-muted-foreground">Usage</dt>
							<dd className="space-y-3" data-testid="claude-usage">
								<div className="flex flex-wrap items-center gap-2">
									<span className="text-muted-foreground text-xs">
										{usageError
											? "Usage unavailable"
											: connection?.state === "connected"
												? usage
													? checkedAt
														? `Updated ${formatDistanceToNow(checkedAt, { addSuffix: true })}`
														: "Subscription allowance"
													: "Loading…"
												: "Connect to view usage"}
									</span>
									<Button
										type="button"
										variant="ghost"
										size="icon"
										className="size-6"
										aria-label={loading ? "Refreshing usage" : "Refresh usage"}
										disabled={loading}
										onClick={() => setRefresh((value) => value + 1)}
									>
										<RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
									</Button>
									<Button variant="outline" size="sm" asChild>
										<a href="https://claude.ai/settings/usage" target="_blank" rel="noopener noreferrer">
											Claude usage <ExternalLink className="size-3" />
										</a>
									</Button>
								</div>
								{windows.map((window) => (
									<div key={window.id} className="max-w-sm space-y-1">
										<div className="flex justify-between gap-3 text-xs">
											<span>{window.label}</span>
											<span>{Math.round(window.remaining)}% left</span>
										</div>
										<Progress value={window.remaining} aria-label={`${window.label} remaining`} aria-valuenow={window.remaining} />
										{window.resetsAt && (
											<p className="text-muted-foreground text-xs" title={new Date(window.resetsAt).toLocaleString()}>
												{new Date(window.resetsAt).getTime() > Date.now()
													? `Resets ${formatDistanceToNow(new Date(window.resetsAt), { addSuffix: true })}`
													: "Reset pending refresh"}
											</p>
										)}
									</div>
								))}
								{usage && windows.length === 0 && <p className="text-muted-foreground text-xs">No usage limits reported</p>}
							</dd>
						</dl>
						<div className="mt-5 flex flex-wrap items-center justify-between gap-3">
							<span className="text-muted-foreground text-xs">Routing weight: {account.weight}</span>
							<div className="flex flex-wrap items-center gap-2">
								<Button type="button" variant="outline" size="sm" disabled={!canUpdate} onClick={onEdit}>
									Edit connection
								</Button>
								{menu}
							</div>
						</div>
					</CollapsibleContent>
				</Collapsible>
			</TableCell>
		</TableRow>
	);
}