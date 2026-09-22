import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { codexAction, type CodexConnection as Connection } from "@/lib/store/apis/codexApi";
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
		if (!key || busy || error || (connection?.state !== "pending" && connection?.state !== "polling")) return;
		const timer = setTimeout(
			() => {
				void run("poll");
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
					<code className="block text-2xl font-semibold tracking-widest">{connection.user_code}</code>
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
				<p className="text-sm" data-testid="codex-connected">
					Connected. Send requests to Bifrost with this virtual key and a <code>codex/</code> model. Bifrost refreshes the subscription
					token automatically.
				</p>
			)}
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