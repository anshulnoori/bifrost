import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useGetProvidersQuery, useGetProviderKeysQuery, useUpdateProviderKeyMutation } from "@/lib/store/apis/providersApi";
import { getErrorMessage } from "@/lib/store";
import { ModelProviderKey } from "@/lib/types/config";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { z } from "zod";

type SubscriptionProvider = "codex" | "claude";
const providerLabel: Record<SubscriptionProvider, string> = { codex: "Codex", claude: "Claude" };

function AccountLimit({ account, provider, canUpdate }: { account: ModelProviderKey; provider: SubscriptionProvider; canUpdate: boolean }) {
	const [value, setValue] = useState(String(account.codex_reserve_percent ?? ""));
	const [error, setError] = useState("");
	const [saved, setSaved] = useState(false);
	const [update, { isLoading }] = useUpdateProviderKeyMutation();
	useEffect(() => {
		setValue(String(account.codex_reserve_percent ?? ""));
	}, [account.codex_reserve_percent]);
	async function save(removeLimit = false) {
		setError("");
		setSaved(false);
		const parsed = z
			.number()
			.min(0)
			.max(100)
			.nullable()
			.safeParse(removeLimit || value.trim() === "" ? null : Number(value));
		if (!parsed.success) {
			setError("Enter a percentage from 0 to 100, or leave blank for no reserve.");
			return;
		}
		try {
			await update({
				provider,
				keyId: account.id,
				key: { ...account, codex_reserve_percent: parsed.data },
			}).unwrap();
			setValue(String(parsed.data ?? ""));
			setSaved(true);
		} catch (error) {
			setError(getErrorMessage(error));
		}
	}
	return (
		<TableRow id={account.id} data-testid={`subscription-limit-${account.id}`}>
			<TableCell className="font-medium">
				{account.name || account.id}
				<div className="text-muted-foreground text-xs">{providerLabel[provider]}</div>
			</TableCell>
			<TableCell>
				<Input
					aria-label={`Weekly reserve for ${account.name || account.id}`}
					type="number"
					min={0}
					max={100}
					step="any"
					placeholder="No reserve"
					value={value}
					disabled={!canUpdate || isLoading}
					className="w-48"
					onChange={(event) => {
						setValue(event.target.value);
						setSaved(false);
					}}
				/>
				{error && (
					<p role="alert" className="text-destructive mt-2 text-sm">
						{error}
					</p>
				)}
			</TableCell>
			<TableCell>
				<Button variant="outline" disabled={!canUpdate || isLoading} onClick={() => save()}>
					{isLoading ? "Saving…" : "Save"}
				</Button>
				{account.codex_reserve_percent != null && (
					<Button
						variant="outline"
						className="ml-2"
						data-testid={`subscription-limit-remove-${account.id}`}
						disabled={!canUpdate || isLoading}
						onClick={() => save(true)}
					>
						Remove limit
					</Button>
				)}
				{saved && (
					<span role="status" className="text-muted-foreground ml-3 text-sm">
						Saved
					</span>
				)}
			</TableCell>
		</TableRow>
	);
}

export default function SubscriptionLimits() {
	const canView = useRbac(RbacResource.ModelProvider, RbacOperation.View);
	const canUpdate = useRbac(RbacResource.ModelProvider, RbacOperation.Update);
	const { data, isLoading, error } = useGetProvidersQuery(undefined, {
		skip: !canView,
	});
	const hasCodex = data?.some((provider) => provider.name === "codex") ?? false;
	const hasClaude = data?.some((provider) => provider.name === "claude") ?? false;
	const codex = useGetProviderKeysQuery("codex", { skip: !canView || !hasCodex });
	const claude = useGetProviderKeysQuery("claude", { skip: !canView || !hasClaude });
	const loadingKeys = codex.isLoading || claude.isLoading;
	const keysError = codex.error || claude.error;
	if (!canView) return <p className="p-4 text-sm">You do not have permission to view provider accounts.</p>;
	if (isLoading || loadingKeys) return <p className="p-4 text-sm">Loading subscription limits…</p>;
	if (error || keysError)
		return (
			<p role="alert" className="text-destructive p-4 text-sm">
				{getErrorMessage(error || keysError)}
			</p>
		);
	const accounts = [
		...(hasCodex ? (codex.data ?? []) : []).map((account) => ({ account, provider: "codex" as const })),
		...(hasClaude ? (claude.data ?? []) : []).map((account) => ({ account, provider: "claude" as const })),
	];
	return (
		<div className="space-y-4 py-4">
			<div>
				<h2 className="text-lg font-medium">Subscription Limits</h2>
				<p className="text-muted-foreground mt-1 text-sm">
					Pause each Codex or Claude subscription account at its weekly allowance reserve. A 25% reserve leaves 25% remaining. The five-hour
					window does not affect this setting. Remove the limit or save a blank field for no reserve. OpenAI and Anthropic still control
					account limits and credit use.
				</p>
			</div>
			{accounts.length ? (
				<Table>
					<TableHeader>
						<TableRow>
							<TableHead>Account</TableHead>
							<TableHead>Weekly allowance reserve (% remaining)</TableHead>
							<TableHead>Actions</TableHead>
						</TableRow>
					</TableHeader>
					<TableBody>
						{accounts.map(({ account, provider }) => (
							<AccountLimit key={`${provider}:${account.id}`} account={account} provider={provider} canUpdate={canUpdate} />
						))}
					</TableBody>
				</Table>
			) : (
				<p className="text-muted-foreground text-sm">
					No Codex or Claude accounts configured. Add an account in Providers to set its subscription limit.
				</p>
			)}
		</div>
	);
}