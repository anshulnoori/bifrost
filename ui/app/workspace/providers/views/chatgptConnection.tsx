import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { chatgptAction, parseChatGPTCallbackUrl, type ChatGPTConnection as Connection } from "@/lib/store/apis/chatgptApi";
import { ExternalLink } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

export default function ChatGPTConnection({ keyId }: { keyId: string }) {
	const [connection, setConnection] = useState<Connection>();
	const [callbackUrl, setCallbackUrl] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const operation = useRef<AbortController | null>(null);
	const id = connection?.id;
	const state = connection?.state ?? "disconnected";

	const run = useCallback(
		async (action: "start" | "status" | "complete" | "disconnect", connectionId?: string) => {
			operation.current?.abort();
			const controller = new AbortController();
			operation.current = controller;
			setBusy(true);
			setError("");
			try {
				const result = await chatgptAction(keyId, action, connectionId, callbackUrl, controller.signal);
				if (!controller.signal.aborted) {
					setConnection((old) => (result.state === "pending" && old?.id === result.id ? { ...old, ...result } : result));
					if (action === "complete" || action === "disconnect" || action === "start") setCallbackUrl("");
				}
			} catch (cause) {
				if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "ChatGPT authorization failed.");
			} finally {
				if (!controller.signal.aborted) setBusy(false);
			}
		},
		[callbackUrl, keyId],
	);

	useEffect(() => {
		void run("status");
		return () => operation.current?.abort();
	}, [keyId]); // run intentionally excluded: callback input must not refetch status

	useEffect(() => {
		if (!id || !["exchanging", "refreshing", "revoking"].includes(state)) return;
		const timer = window.setTimeout(() => void run("status"), 3000);
		return () => window.clearTimeout(timer);
	}, [connection, id, run, state]);

	const authorizationUrl = connection?.authorization_url;
	const canOpenAuthorization = !!authorizationUrl;
	const canComplete = !!id && parseChatGPTCallbackUrlSafe(callbackUrl);

	return (
		<div className="space-y-4" data-testid="chatgpt-onboarding">
			<div className="flex items-center justify-between">
				<h3 className="text-sm font-medium">ChatGPT account</h3>
				<Badge data-testid="chatgpt-status">{state.replaceAll("_", " ")}</Badge>
			</div>
			{state === "pending" && (
				<div className="space-y-4 rounded-md border p-4" data-testid="chatgpt-callback-form">
					<p className="text-muted-foreground text-sm">
						OpenAI requires a local callback address. After approval, your browser may show “This site can’t be reached.” This is expected
						when Bifrost runs remotely. Copy the full address from that tab, paste it below, and select Complete sign-in. Do not replace the
						local address with this dashboard’s address.
					</p>
					<Button asChild disabled={!canOpenAuthorization}>
						<a
							href={canOpenAuthorization ? authorizationUrl : undefined}
							target="_blank"
							rel="noopener noreferrer"
							data-testid="chatgpt-authorization-link"
						>
							Continue with ChatGPT <ExternalLink className="size-4" />
						</a>
					</Button>
					<div className="space-y-2">
						<Label htmlFor="chatgpt-callback-url">Final redirected localhost URL</Label>
						<Input
							id="chatgpt-callback-url"
							value={callbackUrl}
							onChange={(event) => setCallbackUrl(event.target.value)}
							placeholder="http://127.0.0.1:1455/auth/callback?..."
							autoComplete="off"
							spellCheck={false}
							data-testid="chatgpt-callback-url"
						/>
					</div>
					<Button type="button" disabled={busy || !canComplete} onClick={() => void run("complete", id)} data-testid="chatgpt-complete">
						Complete sign-in
					</Button>
				</div>
			)}
			{state === "connected" && (
				<p className="text-muted-foreground text-sm">Connected to ChatGPT{connection?.email ? ` as ${connection.email}` : ""}.</p>
			)}
			{state === "exchanging" && <p className="text-muted-foreground text-sm">Completing sign-in…</p>}
			{state === "revoking" && <p className="text-muted-foreground text-sm">Disconnecting ChatGPT…</p>}
			{state === "expired" && <p className="text-sm">This connection expired. Connect again.</p>}
			{state === "reconnect_required" && <p className="text-sm">Reconnect to continue using this account.</p>}
			{error && (
				<p role="alert" className="text-destructive text-sm" data-testid="chatgpt-error">
					{error}
				</p>
			)}
			<div className="flex flex-wrap gap-2">
				<Button
					type="button"
					variant={state === "connected" ? "outline" : "default"}
					disabled={busy || state === "pending" || state === "exchanging" || state === "refreshing" || state === "revoking"}
					onClick={() => void run("start")}
					data-testid="chatgpt-connect"
				>
					{state === "connected" ? "Reconnect" : "Connect with ChatGPT"}
				</Button>
				{id && state !== "disconnected" && (
					<Button
						type="button"
						variant="outline"
						disabled={busy || state === "revoking"}
						onClick={() => void run("disconnect", id)}
						data-testid="chatgpt-disconnect"
					>
						{state === "pending" ? "Cancel" : "Disconnect"}
					</Button>
				)}
			</div>
		</div>
	);
}

function parseChatGPTCallbackUrlSafe(value: string): boolean {
	try {
		parseChatGPTCallbackUrl(value);
		return true;
	} catch {
		return false;
	}
}