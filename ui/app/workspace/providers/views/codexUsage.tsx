import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { codexAction, codexUsage, type CodexUsage as Usage } from "@/lib/store/apis/codexApi";
import { RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";

export default function CodexUsage({ keyId, revision }: { keyId: string; revision: number }) {
	const [usage, setUsage] = useState<Usage>();
	const [status, setStatus] = useState("Loading…");
	const [refresh, setRefresh] = useState(0);
	useEffect(() => {
		const controller = new AbortController();
		let timer: ReturnType<typeof setTimeout>;
		async function load() {
			try {
				const connection = await codexAction(keyId, "status", undefined, controller.signal);
				if (controller.signal.aborted) return;
				setStatus(connection.state.replaceAll("_", " "));
				if (connection.state === "connected" || connection.state === "refreshing") {
					const result = await codexUsage(keyId, controller.signal);
					if (!controller.signal.aborted) setUsage(result);
				} else setUsage(undefined);
			} catch {
				if (!controller.signal.aborted) {
					setStatus("Usage unavailable");
					setUsage(undefined);
				}
			} finally {
				if (!controller.signal.aborted) timer = setTimeout(load, 60000);
			}
		}
		void load();
		return () => {
			controller.abort();
			clearTimeout(timer);
		};
	}, [keyId, revision, refresh]);
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
	return (
		<div className="mt-2 max-w-sm space-y-2" data-testid={`codex-usage-${keyId}`}>
			<div className="text-muted-foreground flex items-center gap-2 text-xs">
				<span className="capitalize">
					{usage?.plan_type ? `${usage.plan_type} · ` : ""}
					{status}
				</span>
				<Button
					type="button"
					variant="ghost"
					size="icon"
					className="size-5"
					aria-label="Refresh usage"
					onClick={() => setRefresh((value) => value + 1)}
				>
					<RefreshCw className="size-3" />
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
					<div key={`${name}:${index}`} className="space-y-1">
						<div className="flex justify-between gap-3 text-xs">
							<span>
								{name} {label}
							</span>
							<span>{Math.round(remaining)}% left</span>
						</div>
						<Progress value={remaining} aria-label={`${name} ${label} remaining`.trim()} aria-valuenow={remaining} />
						{window.reset_at > 0 && (
							<p className="text-muted-foreground text-xs">Resets {new Date(window.reset_at * 1000).toLocaleString()}</p>
						)}
					</div>
				);
			})}
			{usage && windows.length === 0 && <p className="text-muted-foreground text-xs">No usage limits reported</p>}
			{usage?.rate_limit?.limit_reached && <p className="text-muted-foreground text-xs">Limit reached</p>}
		</div>
	);
}