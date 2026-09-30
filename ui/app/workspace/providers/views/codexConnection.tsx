import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useCopyToClipboard } from "@/hooks/useCopyToClipboard";
import { codexAction, type CodexConnection as Connection } from "@/lib/store/apis/codexApi";
import { useCallback, useEffect, useRef, useState } from "react";

export default function CodexConnection({ keyId }: { keyId: string }) {
	const [connection, setConnection] = useState<Connection>();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const operation = useRef<AbortController | null>(null);
	const { copy, copied } = useCopyToClipboard({ successMessage: "Code copied" });
	const id = connection?.id;
	const run = useCallback(
		async (action: "status" | "start" | "poll" | "disconnect", connectionId?: string) => {
			operation.current?.abort();
			const controller = new AbortController();
			operation.current = controller;
			setBusy(true);
			setError("");
			try {
				const result = await codexAction(keyId, action, connectionId, controller.signal);
				if (!controller.signal.aborted)
					setConnection((old) =>
						(result.state === "pending" || result.state === "polling") && old?.id === result.id ? { ...old, ...result } : result,
					);
			} catch (err) {
				if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "Could not update connection.");
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
	useEffect(() => {
		if (busy || error || !connection || !["pending", "polling", "refreshing"].includes(connection.state)) return;
		const timer = setTimeout(
			() => void run(connection.state === "refreshing" ? "status" : "poll", id),
			(connection.interval_seconds ?? 5) * 1000,
		);
		return () => clearTimeout(timer);
	}, [busy, error, connection, id, run]);
	const state = connection?.state ?? "disconnected";
	const pending = state === "pending" || state === "polling";
	return (
		<div className="space-y-4" data-testid="codex-onboarding">
			<div className="flex items-center justify-between">
				<h3 className="text-sm font-medium">ChatGPT account</h3>
				<Badge variant="outline" data-testid="codex-status">
					{state.replaceAll("_", " ")}
				</Badge>
			</div>
			{pending && connection?.user_code && (
				<div className="space-y-3 rounded-md border p-4" data-testid="codex-device-code">
					<p className="text-sm">Enter this code on OpenAI to connect your account.</p>
					<div className="flex items-center gap-4">
						<code className="text-xl font-semibold tracking-widest">{connection.user_code}</code>
						<Button type="button" variant="outline" size="sm" onClick={() => copy(connection.user_code!)}>
							{copied ? "Copied" : "Copy code"}
						</Button>
					</div>
					<Button asChild variant="outline">
						<a href="https://auth.openai.com/codex/device" target="_blank" rel="noopener noreferrer">
							Continue to OpenAI
						</a>
					</Button>
				</div>
			)}
			{pending && !connection?.user_code && <p className="text-muted-foreground text-sm">Waiting for sign-in. Cancel to get a new code.</p>}
			{state === "connected" && (
				<p className="text-muted-foreground text-sm" data-testid="codex-connected">
					Connected to ChatGPT.
				</p>
			)}
			{state === "expired" && <p className="text-sm">Code expired. Connect again.</p>}
			{state === "reconnect_required" && <p className="text-sm">Reconnect to continue using this account.</p>}
			{error && (
				<p role="alert" className="text-destructive text-sm" data-testid="codex-error">
					{error}
				</p>
			)}
			<div className="flex flex-wrap gap-2">
				<Button
					type="button"
					variant={state === "connected" ? "outline" : "default"}
					disabled={busy || pending}
					onClick={() => void run("start")}
					data-testid="codex-connect"
				>
					{state === "connected" ? "Reconnect" : "Connect with ChatGPT"}
				</Button>
				{id && (
					<Button type="button" variant="outline" disabled={busy} onClick={() => void run("disconnect", id)} data-testid="codex-disconnect">
						{pending ? "Cancel" : "Disconnect"}
					</Button>
				)}
				{error && (
					<Button type="button" variant="outline" disabled={busy} onClick={() => void run("status")} data-testid="codex-check-status">
						Retry
					</Button>
				)}
			</div>
		</div>
	);
}