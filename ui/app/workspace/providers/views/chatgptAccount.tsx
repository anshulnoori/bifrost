import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { TableCell, TableRow } from "@/components/ui/table";
import { chatgptAction, type ChatGPTConnection } from "@/lib/store/apis/chatgptApi";
import { ModelProviderKey } from "@/lib/types/config";
import { ChevronDown, ExternalLink } from "lucide-react";
import { ReactNode, useEffect, useState } from "react";
import { chatgptAccountLabel } from "./chatgptAccountLabel";

interface Props {
	account: ModelProviderKey;
	revision: number;
	canUpdate: boolean;
	onEdit: () => void;
	menu: ReactNode;
}

export default function ChatGPTAccount({ account, revision, canUpdate, onEdit, menu }: Props) {
	const [open, setOpen] = useState(false);
	const [connection, setConnection] = useState<ChatGPTConnection>();

	useEffect(() => {
		const controller = new AbortController();
		void chatgptAction(account.id, "status", undefined, undefined, controller.signal)
			.then(setConnection)
			.catch(() => undefined);
		return () => controller.abort();
	}, [account.id, revision]);

	const status = connection?.state ?? "disconnected";
	const label = chatgptAccountLabel(account.name, connection?.email);
	return (
		<TableRow data-testid={`key-row-${account.name}`} className="hover:bg-transparent">
			<TableCell colSpan={4} className="p-0 whitespace-normal">
				<Collapsible open={open} onOpenChange={setOpen} data-testid={`chatgpt-account-${account.id}`}>
					<CollapsibleTrigger asChild>
						<button
							type="button"
							className="hover:bg-muted/50 relative flex w-full flex-wrap items-center gap-3 px-4 py-4 pr-10 text-left sm:px-5"
							aria-label={`${label} subscription details`}
						>
							<span className="min-w-0 basis-full font-medium [overflow-wrap:anywhere] sm:flex-1">{label}</span>
							<span className="text-muted-foreground text-xs">ChatGPT subscription</span>
							<Badge variant="outline" className="capitalize">
								{status.replaceAll("_", " ")}
							</Badge>
							<ChevronDown className={`absolute top-5 right-4 size-4 transition-transform ${open ? "rotate-180" : ""}`} />
						</button>
					</CollapsibleTrigger>
					<CollapsibleContent className="border-t px-4 py-4 sm:px-5">
						<dl className="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-[auto_minmax(0,1fr)]">
							<dt className="text-muted-foreground">Account</dt>
							<dd data-testid="chatgpt-account-email">{connection?.email ?? "Email unavailable"}</dd>
							<dt className="text-muted-foreground">Models</dt>
							<dd>
								{!account.models?.length || account.models.includes("*") ? "All available ChatGPT models" : account.models.join(", ")}
							</dd>
							<dt className="text-muted-foreground">Billing</dt>
							<dd>Uses your ChatGPT subscription</dd>
							<dt className="text-muted-foreground">Usage</dt>
							<dd>
								<Button asChild variant="link" className="h-auto p-0">
									<a href="https://chatgpt.com/settings/usage" target="_blank" rel="noopener noreferrer">
										Open ChatGPT Settings Usage <ExternalLink className="size-3" />
									</a>
								</Button>
							</dd>
						</dl>
						<div className="mt-5 flex items-center justify-between gap-3">
							<span className="text-muted-foreground text-xs">Routing weight: {account.weight}</span>
							<div className="flex items-center gap-2">
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