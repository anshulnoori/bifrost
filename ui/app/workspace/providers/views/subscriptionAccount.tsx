import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { Progress } from "@/components/ui/progress";
import { TableCell, TableRow } from "@/components/ui/table";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { codexAction, codexUsage, type CodexUsage as Usage } from "@/lib/store/apis/codexApi";
import { chatgptAction } from "@/lib/store/apis/chatgptApi";
import { useUpdateProviderKeyMutation } from "@/lib/store/apis/providersApi";
import { ModelProviderKey } from "@/lib/types/config";
import { getErrorMessage } from "@/lib/store";
import { useVisiblePolling } from "@/hooks/useVisiblePolling";
import { formatDistanceToNow } from "date-fns";
import { ChevronDown, ExternalLink, RefreshCw } from "lucide-react";
import { ReactNode, useState } from "react";
import { toast } from "sonner";
import { subscriptionAccountLabel } from "./subscriptionAccountLabel";

export default function SubscriptionAccount({
	provider,
	account,
	revision,
	canUpdate,
	onEdit,
	onCheck,
	checking,
	menu,
}: {
	provider: "codex" | "chatgpt";
	account: ModelProviderKey;
	revision: number;
	canUpdate: boolean;
	onEdit: () => void;
	onCheck: (label: string) => void;
	checking: boolean;
	menu: ReactNode;
}) {
	const keyId = account.id;
	const [open, setOpen] = useState(false);
	const [updateKey, { isLoading: updating }] = useUpdateProviderKeyMutation();
	const [usage, setUsage] = useState<Usage>();
	const [status, setStatus] = useState("Loading…");
	const [email, setEmail] = useState<string>();
	const [refresh, setRefresh] = useState(0);
	const loading = useVisiblePolling(
		async (signal) => {
			try {
				const connection =
					provider === "codex"
						? await codexAction(keyId, "status", undefined, signal)
						: await chatgptAction(keyId, "status", undefined, undefined, signal);
				if (signal.aborted) return;
				setStatus(connection.state.replaceAll("_", " "));
				setEmail(connection.email);
				if (provider === "codex" && (connection.state === "connected" || connection.state === "refreshing")) {
					const result = await codexUsage(keyId, signal);
					if (!signal.aborted) setUsage(result);
				} else setUsage(undefined);
			} catch {
				if (!signal.aborted) {
					setStatus("Usage unavailable");
					setUsage(undefined);
				}
			}
		},
		[provider, keyId, revision, refresh],
	);
	const groups = usage
		? [
				{ name: "", limits: usage.rate_limit },
				...(usage.additional_rate_limits ?? []).map((item) => ({ name: item.limit_name || item.metered_feature, limits: item.rate_limit })),
			]
		: [];
	const windows = groups.flatMap((group) =>
		[group.limits?.primary_window, group.limits?.secondary_window].flatMap((window, index) =>
			window && window.used_percent !== null ? [{ window, name: group.name, index }] : [],
		),
	);
	// The tightest standard window determines the headline; additional features
	// retain their own limits in the expanded view.
	const primaryWindows = windows.filter(({ name }) => !name);
	const headline = primaryWindows.length
		? Math.max(0, Math.min(100, ...primaryWindows.map(({ window }) => 100 - window.used_percent!)))
		: undefined;
	const enabled = account.enabled ?? true;
	const label = subscriptionAccountLabel(account.name, email, provider);
	const reserve = provider === "codex" ? account.codex_reserve_percent : undefined;
	const weekly = [usage?.rate_limit?.primary_window, usage?.rate_limit?.secondary_window].filter(
		(window) => window?.limit_window_seconds === 604800,
	);
	const weeklyRemaining =
		weekly.length && weekly.every((window) => window?.used_percent != null && window.used_percent >= 0 && window.used_percent <= 100)
			? Math.min(...weekly.map((window) => 100 - window!.used_percent!))
			: undefined;
	const reserveStatus =
		reserve == null || (status !== "connected" && status !== "refreshing")
			? undefined
			: weeklyRemaining === undefined
				? "Usage unavailable"
				: weeklyRemaining <= reserve
					? "Reserve reached"
					: undefined;
	return (
		<TableRow data-testid={`key-row-${account.name}`} className="hover:bg-transparent">
			<TableCell colSpan={4} className="p-0 whitespace-normal">
				<Collapsible
					open={open}
					onOpenChange={setOpen}
					data-testid={`${provider === "codex" ? "codex-usage" : "chatgpt-account"}-${keyId}`}
				>
					<CollapsibleTrigger asChild>
						<button
							type="button"
							className="hover:bg-muted/50 relative flex w-full flex-wrap items-center gap-3 px-4 py-4 pr-10 text-left sm:px-5"
							aria-label={`${label} subscription details`}
						>
							<span className="min-w-0 basis-full font-medium [overflow-wrap:anywhere] sm:flex-1">{label}</span>
							<span className="text-muted-foreground text-xs capitalize">ChatGPT {usage?.plan_type ?? "subscription"}</span>
							{headline !== undefined && (
								<span className="flex items-center gap-2 text-xs tabular-nums">
									<Progress className="h-1.5 w-20" value={headline} aria-label="Remaining allowance" aria-valuenow={headline} />
									<span>{Math.round(headline)}%</span>
								</span>
							)}
							<Badge variant="secondary" className="capitalize">
								{!enabled ? "Inactive" : (reserveStatus ?? status)}
							</Badge>
							<ChevronDown
								className={`absolute top-5 right-4 size-4 shrink-0 transition-transform sm:static ${open ? "rotate-180" : ""}`}
							/>
						</button>
					</CollapsibleTrigger>
					<CollapsibleContent className="border-t px-4 py-4 sm:px-5">
						<dl className="grid grid-cols-1 items-start gap-x-6 gap-y-2 text-sm [overflow-wrap:anywhere] sm:grid-cols-[auto_minmax(0,1fr)] sm:gap-y-4 [&>dt:not(:first-child)]:mt-2 sm:[&>dt:not(:first-child)]:mt-0">
							<dt className="text-muted-foreground pt-1">Account</dt>
							<dd className="flex flex-wrap items-center gap-3">
								<span data-testid={`${provider}-account-email`}>{email?.trim() || "Email unavailable"}</span>
								<Button variant="outline" size="sm" disabled={!canUpdate || checking || !enabled} onClick={() => onCheck(label)}>
									{checking ? "Checking…" : "Check access"}
								</Button>
							</dd>
							<dt className="text-muted-foreground">Models</dt>
							<dd className="break-words">
								{!account.models?.length || account.models.includes("*")
									? `All available ${provider === "codex" ? "Codex" : "ChatGPT"} models`
									: account.models.join(", ")}
								{!!account.blacklisted_models?.length && (
									<span className="text-muted-foreground"> · Excludes {account.blacklisted_models.join(", ")}</span>
								)}
							</dd>
							<dt className="text-muted-foreground">Billing</dt>
							<dd>Uses your ChatGPT subscription allowance</dd>
							{provider === "codex" && (
								<>
									<dt className="text-muted-foreground">Routing reserve</dt>
									<dd>
										{reserve == null ? "No reserve" : `Pause at ${reserve}% weekly allowance remaining`} ·{" "}
										<a
											className="text-primary underline"
											href={`/workspace/model-limits?tab=subscriptions#${encodeURIComponent(account.id)}`}
										>
											Edit in Budgets & Limits
										</a>
									</dd>
								</>
							)}
							<dt className="text-muted-foreground">Usage</dt>
							<dd className="space-y-3">
								<div className="flex flex-wrap items-center gap-2">
									<span className="text-muted-foreground text-xs capitalize">{status}</span>
									{usage?.checked_at && (
										<span className="text-muted-foreground text-xs" data-testid="usage-checked-at">
											Updated {formatDistanceToNow(new Date(usage.checked_at), { addSuffix: true })}
										</span>
									)}
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
										<a
											href={provider === "codex" ? "https://chatgpt.com/codex/settings/usage" : "https://chatgpt.com/settings/usage"}
											target="_blank"
											rel="noopener noreferrer"
										>
											ChatGPT usage <ExternalLink className="size-3" />
										</a>
									</Button>
								</div>
								{windows.map(({ window, name, index }) => {
									const remaining = Math.max(0, Math.min(100, 100 - window.used_percent!));
									const hours = window.limit_window_seconds / 3600;
									const label =
										hours >= 24
											? `${Math.round(hours / 24)}d`
											: hours > 0
												? `${Math.round(hours * 10) / 10}h`
												: index === 0
													? "Primary"
													: "Secondary";
									return (
										<div key={`${name}:${index}`} className="max-w-sm space-y-1">
											<div className="flex justify-between gap-3 text-xs">
												<span>
													{name} {label}
												</span>
												<span>{Math.round(remaining)}% left</span>
											</div>
											<Progress value={remaining} aria-label={`${name} ${label} remaining`.trim()} aria-valuenow={remaining} />
											{window.reset_at > 0 && (
												<p className="text-muted-foreground text-xs" title={new Date(window.reset_at * 1000).toLocaleString()}>
													{window.reset_at * 1000 > Date.now()
														? `Resets ${formatDistanceToNow(window.reset_at * 1000, { addSuffix: true })}`
														: "Reset pending refresh"}
												</p>
											)}
										</div>
									);
								})}
								{usage && windows.length === 0 && <p className="text-muted-foreground text-xs">No usage limits reported</p>}
								{usage?.rate_limit?.limit_reached && <p className="text-muted-foreground text-xs">Limit reached</p>}
							</dd>
						</dl>
						<div className="mt-5 flex flex-wrap items-center justify-between gap-3">
							<span className="text-muted-foreground text-xs">Routing weight: {account.weight}</span>
							<div className="flex flex-wrap items-center gap-2">
								<Button
									variant="outline"
									size="sm"
									disabled={!canUpdate || updating}
									onClick={async () => {
										try {
											await updateKey({ provider, keyId, key: { ...account, enabled: !enabled } }).unwrap();
										} catch (error) {
											toast.error("Could not update account", { description: getErrorMessage(error) });
										}
									}}
								>
									{enabled ? "Deactivate" : "Activate"}
								</Button>
								<Button variant="outline" size="sm" disabled={!canUpdate} onClick={onEdit}>
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