import { useEffect, useState } from "react";

// Subscription usage endpoints are rate-limited upstream. The gateway caches
// readings for 1 minute, so the dashboard polls at that cadence, with jitter so
// several rows and tabs do not ask together, and only while the page is visible.
export const SUBSCRIPTION_POLL_MS = 60 * 1000;

// Returns whether a load is in flight, so refresh controls can show progress.
export function useVisiblePolling(
	load: (signal: AbortSignal) => Promise<void>,
	deps: readonly unknown[],
	intervalMs = SUBSCRIPTION_POLL_MS,
) {
	const [loading, setLoading] = useState(false);
	useEffect(() => {
		const controller = new AbortController();
		let timer: ReturnType<typeof setTimeout> | undefined;
		let lastRun = 0;
		const schedule = () => {
			clearTimeout(timer);
			if (controller.signal.aborted || document.visibilityState !== "visible") return;
			timer = setTimeout(run, intervalMs * (0.9 + Math.random() * 0.2));
		};
		const run = async () => {
			lastRun = Date.now();
			setLoading(true);
			try {
				await load(controller.signal);
			} finally {
				if (!controller.signal.aborted) setLoading(false);
				schedule();
			}
		};
		const onVisibility = () => {
			if (document.visibilityState !== "visible") return clearTimeout(timer);
			// Returning to a stale page refreshes once; otherwise resume the cadence.
			if (Date.now() - lastRun >= intervalMs) void run();
			else schedule();
		};
		void run();
		document.addEventListener("visibilitychange", onVisibility);
		return () => {
			controller.abort();
			clearTimeout(timer);
			document.removeEventListener("visibilitychange", onVisibility);
		};
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [...deps, intervalMs]);
	return loading;
}