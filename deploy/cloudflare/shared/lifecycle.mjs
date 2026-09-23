// Storage transaction is the serialization boundary; never retry inference.
export async function admit(storage, headers, now = Date.now()) {
  return storage.transaction(async tx => {
    const key = headers.get('x-deployment-key');
    const ip = headers.get('x-deployment-ip');
    const replay = headers.get('x-deployment-replay');
    if (!/^[a-f0-9]{64}$/.test(key ?? '') || !/^[a-f0-9]{64}$/.test(ip ?? '')) return 403;
    const rpm = Number(headers.get('x-deployment-rpm'));
    const daily = Number(headers.get('x-deployment-daily'));
    if (!Number.isInteger(rpm) || rpm < 1 || rpm > 120 || !Number.isInteger(daily) || daily < 1) return 403;
    if (replay && (await tx.get(`replay:${replay}`) ?? 0) > now) return 409;
    for (const [name, window, cap] of [[`key:${key}`, 60000, rpm], [`ip:${ip}`, 60000, 180], [`day:${key}`, 86400000, daily]]) {
      const record = await tx.get(name);
      if (record && record.until > now && record.count >= cap) return 429;
    }
    for (const [name, window] of [[`key:${key}`, 60000], [`ip:${ip}`, 60000], [`day:${key}`, 86400000]]) {
      const record = await tx.get(name);
      await tx.put(name, record && record.until > now ? { ...record, count: record.count + 1 } : { count: 1, until: now + window });
    }
    if (replay) await tx.put(`replay:${replay}`, now + 86400000);
    return 200;
  });
}

export function drainBody(body, done, signal) {
  if (!body) { void done(); return null; }
  const reader = body.getReader();
  let finished = false;
  const finish = async () => { if (!finished) { finished = true; signal?.removeEventListener('abort', abort); await done(); } };
  const abort = () => { void reader.cancel().catch(() => {}).finally(finish); };
  signal?.addEventListener('abort', abort, { once: true });
  if (signal?.aborted) abort();
  return new ReadableStream({
    async pull(controller) {
      try {
        const item = await reader.read();
        if (item.done) { controller.close(); await finish(); }
        else controller.enqueue(item.value);
      } catch (error) { controller.error(error); await finish(); }
    },
    async cancel(reason) { try { await reader.cancel(reason); } finally { await finish(); } },
  });
}
