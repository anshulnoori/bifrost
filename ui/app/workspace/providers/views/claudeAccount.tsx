import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { TableCell, TableRow } from "@/components/ui/table";
import { claudeAction, type ClaudeConnection as Connection } from "@/lib/store/apis/claudeApi";
import { ModelProviderKey } from "@/lib/types/config";
import { ChevronDown } from "lucide-react";
import { ReactNode, useEffect, useState } from "react";
import { codexAccountLabel } from "./codexAccountLabel";

export default function ClaudeAccount({
	account,
	revision,
	canUpdate,
	onEdit,
	menu,
}: {
	account: ModelProviderKey;
	revision: number;
	canUpdate: boolean;
	onEdit: () => void;
	menu: ReactNode;
}) {
	const [open, setOpen] = useState(false);
	const [connection, setConnection] = useState<Connection>();
	const [error, setError] = useState("");
	useEffect(() => {
		const controller = new AbortController();
		const refresh = () =>
			claudeAction(account.id, "status", controller.signal)
				.then((result) => {
					if (!controller.signal.aborted) {
						setConnection(result);
						setError("");
					}
				})
				.catch(() => {
					if (!controller.signal.aborted) setError("Status unavailable");
				});
		void refresh();
		const timer = setInterval(() => {
			void refresh();
		}, 30000);
		return () => {
			clearInterval(timer);
			controller.abort();
		};
	}, [account.id, revision]);
	const label = codexAccountLabel(account.name, connection?.email, "claude");
	const status = account.enabled === false ? "Inactive" : error || connection?.state.replaceAll("_", " ") || "Loading…";
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
							<Badge variant="secondary" className="capitalize">
								{status}
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
								{!account.models?.length || account.models.includes("*") ? "All models" : account.models.join(", ")}
								{!!account.blacklisted_models?.length && (
									<span className="text-muted-foreground"> · Excluded: {account.blacklisted_models.join(", ")}</span>
								)}
							</dd>
							<dt className="text-muted-foreground">Billing</dt>
							<dd>Uses Claude subscription allowance</dd>
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
