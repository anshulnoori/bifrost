import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { claudeAction, type ClaudeConnection as Connection } from "@/lib/store/apis/claudeApi";
import { useCallback, useEffect, useRef, useState } from "react";

export default function ClaudeConnection({ keyId }: { keyId: string }) {
	const [connection, setConnection] = useState<Connection>();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [code, setCode] = useState("");
	const operation = useRef<AbortController | null>(null);
	const run = useCallback(
		async (action: "status" | "start" | "code" | "disconnect", submitted?: { id: string; code: string }, silent = false) => {
			operation.current?.abort();
			const controller = new AbortController();
			operation.current = controller;
			setBusy(true);
			if (!silent) setError("");
			try {
				const result = await claudeAction(keyId, action, controller.signal, submitted);
				if (!controller.signal.aborted) {
					setConnection(result);
					if (result.state === "connected") setError("");
					if (action !== "status") setCode("");
				}
			} catch (err) {
				if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "Claude account operation failed");
			} finally {
				if (!controller.signal.aborted) setBusy(false);
			}
		},
		[keyId],
	);
	useEffect(() => {
		void run("status");
		return () => operation.current?.abort();
	}, [run]);
	const state = connection?.state ?? "disconnected";
	const pending = state === "pending" || state === "connecting";
	useEffect(() => {
		if (!pending || busy) return;
		const timer = setTimeout(
			() => {
				void run("status", undefined, true);
			},
			(connection?.interval_seconds ?? 2) * 1000,
		);
		return () => clearTimeout(timer);
	}, [pending, busy, connection, run]);
	return (
		<div className="space-y-4" data-testid="claude-onboarding">
			<div className="flex items-center justify-between">
				<h3 className="text-sm font-medium">Claude account</h3>
				<Badge variant={state === "connected" ? "default" : "secondary"} data-testid="claude-status">
					{state.replaceAll("_", " ")}
				</Badge>
			</div>
			{pending && connection?.authorization_url && (
				<div className="space-y-3 rounded-md border p-4" data-testid="claude-browser-login">
					<p className="text-sm">Sign in to Claude in a new tab. Copy the complete authorization code back here.</p>
					<Button type="button" asChild>
						<a href={connection.authorization_url} target="_blank" rel="noopener noreferrer">
							Open Claude sign-in
						</a>
					</Button>
					<div className="space-y-2">
						<label htmlFor={`claude-code-${keyId}`} className="text-sm font-medium">
							Authorization code
						</label>
						<Input
							id={`claude-code-${keyId}`}
							type="password"
							autoComplete="off"
							placeholder="code#state"
							value={code}
							onChange={(event) => setCode(event.target.value)}
							disabled={state !== "pending"}
							data-testid="claude-authorization-code"
						/>
						<Button
							type="button"
							variant="outline"
							disabled={busy || !code.trim() || state !== "pending"}
							onClick={() => {
								if (connection.id) void run("code", { id: connection.id, code });
							}}
							data-testid="claude-submit-code"
						>
							Complete sign-in
						</Button>
					</div>
				</div>
			)}
			{state === "connecting" && <p className="text-muted-foreground text-sm">Completing sign-in…</p>}
			{state === "connected" && (
				<p className="text-muted-foreground text-sm" data-testid="claude-connected">
					Connected to Claude{connection?.email ? ` as ${connection.email}` : ""}.
				</p>
			)}
			{state === "expired" && <p className="text-sm">Login expired. Start again.</p>}
			{state === "reconnect_required" && <p className="text-sm">Reconnect this Claude account.</p>}
			{error && (
				<p className="text-destructive text-sm" role="alert" data-testid="claude-error">
					{error}
				</p>
			)}
			<div className="flex flex-wrap gap-2">
				<Button
					type="button"
					variant={state === "connected" ? "outline" : "default"}
					disabled={busy || pending}
					onClick={() => {
						void run("start");
					}}
					data-testid="claude-connect"
				>
					{state === "connected" ? "Reconnect" : "Connect Claude"}
				</Button>
				{(pending || state === "connected") && (
					<Button
						type="button"
						variant="outline"
						disabled={busy}
						onClick={() => {
							void run("disconnect");
						}}
						data-testid="claude-disconnect"
					>
						{pending ? "Cancel" : "Disconnect"}
					</Button>
				)}
				{error && (
					<Button
						type="button"
						variant="outline"
						disabled={busy}
						onClick={() => {
							void run("status");
						}}
					>
						Retry
					</Button>
				)}
			</div>
		</div>
	);
}
