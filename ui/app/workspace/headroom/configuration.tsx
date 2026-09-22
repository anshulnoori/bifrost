import { useState } from "react";
import { z } from "zod";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useCreatePluginMutation, useGetPluginsQuery, useUpdatePluginMutation } from "@/lib/store/apis/pluginsApi";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import type { Plugin } from "@/lib/types/plugins";

const configSchema = z
	.object({
		enabled: z.boolean(),
		scope: z.enum(["gateway", "restricted"]),
		virtual_key_id: z.string().trim(),
		project_id: z.string().trim(),
		endpoint: z.url().refine((value) => {
			const url = new URL(value);
			return (
				["http:", "https:"].includes(url.protocol) && !url.username && !url.password && !url.search && !url.hash && url.pathname === "/"
			);
		}, "Use an HTTP(S) origin without a path or credentials."),
		failure_policy: z.enum(["open", "closed"]),
		timeout_ms: z.coerce.number().int().min(1).max(30000),
		retention_seconds: z.coerce.number().int().min(0).max(86400),
	})
	.refine((value) => !value.enabled || value.scope === "gateway" || !!value.virtual_key_id || !!value.project_id, {
		message: "Select a virtual key ID or project ID before enabling compression.",
		path: ["virtual_key_id"],
	});

export default function HeadroomConfiguration() {
	const { data, isLoading, error, refetch } = useGetPluginsQuery();
	const plugin = data?.find((item) => item.name === "headroom");
	if (isLoading) return <p className="text-muted-foreground text-sm">Loading configuration…</p>;
	if (error)
		return (
			<div role="alert">
				Unable to load plugin configuration.{" "}
				<Button variant="outline" onClick={() => refetch()}>
					Retry
				</Button>
			</div>
		);
	return <ConfigurationForm plugin={plugin} />;
}

function ConfigurationForm({ plugin }: { plugin?: Plugin }) {
	const config = plugin?.config ?? {};
	const [enabled, setEnabled] = useState(Boolean(config.enabled));
	const [error, setError] = useState("");
	const [saved, setSaved] = useState(false);
	const [create, creating] = useCreatePluginMutation();
	const [update, updating] = useUpdatePluginMutation();
	const allowed = useRbac(RbacResource.Plugins, plugin ? RbacOperation.Update : RbacOperation.Create);
	const busy = creating.isLoading || updating.isLoading;
	return (
		<Card>
			<CardHeader>
				<CardTitle>Configuration</CardTitle>
			</CardHeader>
			<CardContent>
				<form
					className="space-y-4"
					onSubmit={async (event) => {
						event.preventDefault();
						if (!allowed || busy) return;
						setError("");
						setSaved(false);
						const values = Object.fromEntries(new FormData(event.currentTarget));
						const parsed = configSchema.safeParse({ ...values, enabled });
						if (!parsed.success) {
							setError(parsed.error.issues[0].message);
							return;
						}
						const path = String(values.path ?? "").trim();
						if (!path.startsWith("/") || !path.endsWith(".so")) {
							setError("Enter the absolute path of the installed .so plugin.");
							return;
						}
						const data = {
							enabled: true,
							path,
							placement: "post_builtin",
							order: plugin?.order ?? 0,
							config: {
								...config,
								...parsed.data,
								ccr: false,
								token_env: "HEADROOM_PROXY_TOKEN",
								scope_key_env: "HEADROOM_SCOPE_KEY",
								metrics_address: "127.0.0.1:9909",
								metrics_token_env: "HEADROOM_METRICS_TOKEN",
							},
						};
						try {
							if (plugin)
								await update({
									name: "headroom",
									data: { ...data, path: path === plugin.path ? undefined : path },
								}).unwrap();
							else await create({ name: "headroom", ...data }).unwrap();
							setSaved(true);
						} catch {
							setError("Unable to save configuration. Check that the plugin is installed and the gateway secrets are configured.");
						}
					}}
				>
					<div className="flex items-center gap-3">
						<Switch id="headroom-enabled" checked={enabled} onCheckedChange={setEnabled} disabled={!allowed || busy} />
						<Label htmlFor="headroom-enabled">Enable compression</Label>
					</div>
					<fieldset disabled={!allowed || busy} className="grid gap-4 md:grid-cols-2">
						<div className="space-y-2 md:col-span-2">
							<Label htmlFor="headroom-scope">Apply to</Label>
							<select
								id="headroom-scope"
								name="scope"
								defaultValue={config.scope ?? (config.project_id || config.virtual_key_id ? "restricted" : "gateway")}
								className="border-input bg-background h-9 w-full rounded-sm border px-3 text-sm"
							>
								<option value="gateway">All admitted requests on this gateway</option>
								<option value="restricted">Selected virtual key or project</option>
							</select>
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-key">Virtual key ID (optional restriction)</Label>
							<Input
								id="headroom-key"
								name="virtual_key_id"
								defaultValue={config.virtual_key_id ?? ""}
								placeholder="ID of the key used for inference"
							/>
							<p className="text-muted-foreground text-xs">
								Leave blank to use your normal provider configuration. An ID restricts compression to that virtual key.
							</p>
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-project">Project ID (optional)</Label>
							<Input id="headroom-project" name="project_id" defaultValue={config.project_id ?? ""} />
							<p className="text-muted-foreground text-xs">If both are set, the request must match both.</p>
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-endpoint">Headroom service</Label>
							<Input id="headroom-endpoint" name="endpoint" defaultValue={config.endpoint ?? "http://127.0.0.1:8787"} required />
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-path">Installed plugin path</Label>
							<Input id="headroom-path" name="path" defaultValue={plugin?.path ?? "/tmp/headroom-combined.so"} required />
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-timeout">Compression timeout (ms)</Label>
							<Input
								id="headroom-timeout"
								name="timeout_ms"
								type="number"
								min={1}
								max={30000}
								defaultValue={config.timeout_ms ?? 2000}
								required
							/>
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-retention">Event retention (seconds)</Label>
							<Input
								id="headroom-retention"
								name="retention_seconds"
								type="number"
								min={0}
								max={86400}
								defaultValue={config.retention_seconds ?? 900}
								required
							/>
						</div>
						<div className="space-y-2">
							<Label htmlFor="headroom-policy">If compression fails</Label>
							<select
								id="headroom-policy"
								name="failure_policy"
								defaultValue={config.failure_policy ?? "open"}
								className="border-input bg-background h-9 w-full rounded-sm border px-3 text-sm"
							>
								<option value="open">Continue with original request</option>
								<option value="closed">Reject request</option>
							</select>
						</div>
					</fieldset>
					<p className="text-muted-foreground text-xs">
						Uses your client's session ID when available, otherwise the request ID. Set x-headroom-thread to separate threads within a
						session. The gateway reads credentials from its environment; credentials are never saved in this form.
					</p>
					{error && (
						<p role="alert" className="text-destructive text-sm">
							{error}
						</p>
					)}
					{saved && (
						<p role="status" className="text-sm">
							Configuration saved.
						</p>
					)}
					<Button type="submit" disabled={!allowed || busy} isLoading={busy}>
						Save configuration
					</Button>
				</form>
			</CardContent>
		</Card>
	);
}