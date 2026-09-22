import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useCopyToClipboard } from "@/hooks/useCopyToClipboard";
import { codexAction, codexModels, codexProbe, type CodexConnection as Connection } from "@/lib/store/apis/codexApi";
import { codexGatewayKeySchema } from "@/lib/types/schemas";
import { zodResolver } from "@hookform/resolvers/zod";
import { useCallback, useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";

export default function CodexConnection() {
	const form = useForm<{ key: string }>({
		resolver: zodResolver(codexGatewayKeySchema),
		defaultValues: { key: "" },
	});
	const [key, setKey] = useState("");
	const [connection, setConnection] = useState<Connection>();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const operation = useRef<AbortController | null>(null);
	const { copy, copied } = useCopyToClipboard({ successMessage: "Device code copied" });

	useEffect(() => () => operation.current?.abort(), []);

	const run = useCallback(
		async (action: "status" | "start" | "poll" | "disconnect", gatewayKey = key) => {
			operation.current?.abort();
			const controller = new AbortController();
			operation.current = controller;
			setBusy(true);
			setError("");
			try {
				const result = await codexAction(gatewayKey, action, connection?.id, controller.signal);
				if (controller.signal.aborted) return;
				setConnection((old) =>
					(result.state === "pending" || result.state === "polling") && old?.id === result.id ? { ...old, ...result } : result,
				);
			} catch (err) {
				if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "Could not update the Codex connection.");
			} finally {
				if (!controller.signal.aborted) setBusy(false);
			}
		},
		[key, connection?.id],
	);

	// One poll at a time. Closing the page stops polling; expiry is enforced by
	// the server, and Cancel deletes the pending authorization on every replica.
	useEffect(() => {
		if (!key || busy || error || !connection || !["pending", "polling", "refreshing"].includes(connection.state)) return;
		const timer = setTimeout(
			() => {
				void run(connection.state === "refreshing" ? "status" : "poll");
			},
			(connection.interval_seconds ?? 5) * 1000,
		);
		return () => clearTimeout(timer);
	}, [key, busy, error, connection, run]);

	const state = connection?.state ?? "disconnected";
	const pending = state === "pending" || state === "polling";
	return (
		<section className="mx-auto w-full max-w-2xl space-y-6 rounded-lg border p-6" data-testid="codex-onboarding">
			<div className="flex items-center justify-between gap-4">
				<div>
					<h2 className="text-lg font-semibold">Connect your ChatGPT subscription</h2>
					<p className="text-muted-foreground mt-1 text-sm">A private connection for one Bifrost virtual key.</p>
				</div>
				<Badge variant="outline" data-testid="codex-status">
					{state.replaceAll("_", " ")}
				</Badge>
			</div>
			<div className="bg-muted rounded-md p-4 text-sm">
				Use only your own eligible subscription for permitted coding work. This integration is not endorsed by OpenAI. Subscription limits
				still apply. Never enter an OpenAI access token, refresh token, or password here.
			</div>
			<form
				className="space-y-3"
				onSubmit={form.handleSubmit(({ key: value }) => {
					setConnection(undefined);
					setKey(value);
					void run("status", value);
				})}
			>
				<Label htmlFor="codex-gateway-key">Bifrost virtual key</Label>
				<Input
					id="codex-gateway-key"
					type="password"
					autoComplete="off"
					spellCheck={false}
					{...form.register("key")}
					data-testid="codex-gateway-key"
					placeholder="A gateway virtual key allowing Codex"
				/>
				{form.formState.errors.key && (
					<p role="alert" className="text-destructive text-sm">
						{form.formState.errors.key.message}
					</p>
				)}
				<p className="text-muted-foreground text-xs">
					Kept in memory for this page only. Use this same virtual key for inference. Do not share it.
				</p>
				<Button type="submit" variant="outline" disabled={busy} data-testid="codex-check-status">
					Check connection
				</Button>
			</form>
			{error && (
				<p role="alert" className="text-destructive text-sm" data-testid="codex-error">
					{error}
				</p>
			)}
			{pending && connection?.user_code && (
				<div className="space-y-3 rounded-md border p-4" data-testid="codex-device-code">
					<p className="text-sm">Open OpenAI’s sign-in page and enter this one-time code. Continue only if you started this login.</p>
					<div className="flex items-center gap-4">
						<code className="block text-2xl font-semibold tracking-widest">{connection.user_code}</code>
						<Button variant="outline" size="sm" onClick={() => copy(connection.user_code!)}>
							{copied ? "Copied" : "Copy code"}
						</Button>
					</div>
					<a className="text-primary underline" href="https://auth.openai.com/codex/device" target="_blank" rel="noopener noreferrer">
						Open OpenAI device authorization
					</a>
					<p className="text-muted-foreground text-xs">
						Expires at {connection.expires_at ? new Date(connection.expires_at).toLocaleTimeString() : "the server deadline"}. Enable device
						authorization in ChatGPT security settings if needed.
					</p>
				</div>
			)}
			{pending && !connection?.user_code && (
				<p className="text-sm">
					Authorization is pending. If you no longer have the device code, cancel this authorization and connect again.
				</p>
			)}
			{state === "connected" && (
				<div className="space-y-4">
					<p className="text-sm" data-testid="codex-connected">
						Connected to ChatGPT. Bifrost refreshes your subscription token automatically.
					</p>
					<CodexVerification key={connection?.id} gatewayKey={key} />
				</div>
			)}
			{state === "expired" && <p className="text-sm">The device code expired. Connect again to get a new code.</p>}
			{state === "reconnect_required" && (
				<p className="text-sm">
					Authorization could not be safely refreshed. Reconnect to continue; Bifrost will not reuse a potentially consumed refresh token.
				</p>
			)}
			{key && (
				<div className="flex flex-wrap gap-3">
					<Button disabled={busy || pending} onClick={() => void run("start")} data-testid="codex-connect">
						{state === "disconnected" ? "Connect with ChatGPT" : "Reconnect with ChatGPT"}
					</Button>
					{connection?.id && (
						<Button variant="outline" disabled={busy} onClick={() => void run("disconnect")} data-testid="codex-disconnect">
							{pending ? "Cancel authorization" : "Disconnect"}
						</Button>
					)}
				</div>
			)}
			<p className="text-muted-foreground text-xs">
				Reconnect replaces the previous connection. Disconnect removes stored credentials from Bifrost; it does not revoke the session at
				OpenAI or cancel inference already sent upstream.
			</p>
		</section>
	);
}

function CodexVerification({ gatewayKey }: { gatewayKey: string }) {
	const [models, setModels] = useState<string[]>([]);
	const [model, setModel] = useState("");
	const [loading, setLoading] = useState(true);
	const [testing, setTesting] = useState(false);
	const [attempt, setAttempt] = useState(0);
	const [error, setError] = useState("");
	const [result, setResult] = useState<{ text: string; tokens?: number }>();
	const operation = useRef<AbortController | null>(null);
	useEffect(() => () => operation.current?.abort(), []);
	useEffect(() => {
		const controller = new AbortController();
		operation.current = controller;
		setLoading(true);
		setError("");
		void codexModels(gatewayKey, controller.signal)
			.then((available) => {
				if (controller.signal.aborted) return;
				setModels(available);
				setModel(available[0] ?? "");
			})
			.catch((err: unknown) => {
				if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "Could not load models.");
			})
			.finally(() => {
				if (!controller.signal.aborted) setLoading(false);
			});
		return () => controller.abort();
	}, [gatewayKey, attempt]);

	async function verify() {
		operation.current?.abort();
		const controller = new AbortController();
		operation.current = controller;
		setTesting(true);
		setError("");
		setResult(undefined);
		try {
			const response = await codexProbe(gatewayKey, model, controller.signal);
			if (!controller.signal.aborted) setResult(response);
		} catch (err) {
			if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "The test request failed.");
		} finally {
			if (!controller.signal.aborted) setTesting(false);
		}
	}
	return (
		<div className="space-y-3 rounded-md border p-4" data-testid="codex-verification">
			<h3 className="font-medium">Verify inference</h3>
			{loading ? (
				<p className="text-muted-foreground text-sm">Loading your available models…</p>
			) : models.length === 0 ? (
				<p className="text-sm">No models are available for this connection. Check your plan and virtual-key permissions.</p>
			) : (
				<>
					<Label htmlFor="codex-test-model">Available model</Label>
					<Select
						value={model}
						onValueChange={(value) => {
							setModel(value);
							setResult(undefined);
						}}
						disabled={testing}
					>
						<SelectTrigger id="codex-test-model" className="w-full">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							{models.map((id) => (
								<SelectItem key={id} value={id}>
									{id}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<p className="text-muted-foreground text-xs">
						Sends one short request through normal gateway authentication and budgets. Uses your subscription allowance.
					</p>
					<Button onClick={() => void verify()} disabled={testing || !model} data-testid="codex-test-inference">
						{testing ? "Testing…" : "Send test request"}
					</Button>
					<div className="space-y-1 text-xs">
						<p>
							OpenAI-compatible base URL: <code className="break-all">{window.location.origin}/v1</code>
						</p>
						<p>
							Model: <code>{model}</code> · API key: this Bifrost virtual key, not an OpenAI token.
						</p>
					</div>
				</>
			)}
			{error && (
				<p role="alert" className="text-destructive text-sm">
					{error}
				</p>
			)}
			{!loading && models.length === 0 && (
				<Button variant="outline" onClick={() => setAttempt((value) => value + 1)}>
					Retry model discovery
				</Button>
			)}
			{result && (
				<div className="space-y-1 text-sm" data-testid="codex-test-success">
					<p className="font-medium">
						Inference verified{result.tokens !== undefined ? ` · ${result.tokens} tokens` : " · usage not reported"}
					</p>
					<p>{result.text}</p>
				</div>
			)}
		</div>
	);
}