import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ModelProvider } from "@/lib/types/config";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { SettingsIcon, Trash } from "lucide-react";
import { useMemo, useState } from "react";
import ProviderConfigSheet from "../dialogs/providerConfigSheet";
import ModelProviderKeysTableView from "./modelProviderKeysTableView";

interface Props {
	provider: ModelProvider;
	onRequestDelete?: () => void;
}

export default function ModelProviderConfig({ provider, onRequestDelete }: Props) {
	const [showConfigSheet, setShowConfigSheet] = useState(false);
	const hasDeleteProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Delete);
	const hasUpdateProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Update);

	const showApiKeys = useMemo(() => {
		if (provider.custom_provider_config) {
			return !(provider.custom_provider_config?.is_key_less ?? false);
		}
		return true;
	}, [provider.name, provider.custom_provider_config?.is_key_less]);

	const editConfigButton = (
		<div className="flex min-w-0 flex-wrap items-center gap-2">
			{onRequestDelete && hasDeleteProviderAccess && (
				<Button
					variant="outline"
					onClick={onRequestDelete}
					className="text-destructive hover:bg-destructive/10 hover:text-destructive size-9 px-0"
					aria-label="Delete provider"
					data-testid="provider-delete-btn"
				>
					<Trash className="h-4 w-4" />
				</Button>
			)}
			<Tooltip>
				<TooltipTrigger asChild>
					<Button
						variant="outline"
						className="size-9 px-0 xl:h-9 xl:w-auto xl:px-4"
						onClick={() => setShowConfigSheet(true)}
						aria-label={hasUpdateProviderAccess ? "Edit provider configuration" : "View provider configuration"}
					>
						<SettingsIcon className="h-4 w-4" />
						<span className="hidden xl:inline">{hasUpdateProviderAccess ? "Edit Provider Config" : "View Provider Config"}</span>
					</Button>
				</TooltipTrigger>
				<TooltipContent className="xl:hidden">{hasUpdateProviderAccess ? "Edit Provider Config" : "View Provider Config"}</TooltipContent>
			</Tooltip>
		</div>
	);

	return (
		<div className="flex w-full flex-col gap-2">
			<ProviderConfigSheet show={showConfigSheet} onCancel={() => setShowConfigSheet(false)} provider={provider} />
			{provider.name === "claude" ? (
				<Card data-testid="claude-provider-guidance">
					<CardHeader className="flex flex-row items-center justify-between gap-2">
						<CardTitle>Claude</CardTitle>
						{editConfigButton}
					</CardHeader>
					<CardContent className="text-muted-foreground space-y-3 text-sm">
						<p>Uses a pinned, modified Claude runtime. Configure the owner login on the bridge host, not in Bifrost.</p>
						<p>
							Send native history, system blocks, and caller tools to /v1/messages with a claude/ model prefix. Each request is independent.
						</p>
						<p>
							Native streaming is supported. The bridge does not execute tools. OpenAI endpoints and session continuation are not supported.
						</p>
					</CardContent>
				</Card>
			) : (
				<ModelProviderKeysTableView provider={provider} headerActions={editConfigButton} isKeyless={!showApiKeys} />
			)}
		</div>
	);
}