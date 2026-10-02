// Build-specific imports must remain dynamic: Bun's static import table belongs
// to the original entry. Login and credential refresh use the original client.
if (!process.env.CLAUDE_CONFIG_DIR) throw new Error("set an owner-specific CLAUDE_CONFIG_DIR");
const startup = setInterval(() => {}, 1000);
// __BIFROST_BRIDGE__
const server = createBridge(
  { token: process.env.CLAUDE_BRIDGE_TOKEN ?? "" },
  async ({ body, headers, signal }) => {
    const { tD } = await import("/$bunfs/root/chunk-da9jta6b.js");
    const { startMdmRawRead } = await import("/$bunfs/root/chunk-cd7krv4h.js");
    const { startKeychainPrefetch } = await import("/$bunfs/root/chunk-y8e2dq66.js");
    startMdmRawRead();
    startKeychainPrefetch();
    const { enableConfigs } = await import("/$bunfs/root/chunk-ktfrs76h.js");
    await enableConfigs();
    const { cp } = await import("/$bunfs/root/chunk-djntk4j0.js");
    const client = await tD({
      maxRetries: 0,
      model: body.model,
      source: "bifrost",
      agentContext: { agentType: "main", isBackgroundAgent: false },
      signal,
    });
    const beta = [headers["anthropic-beta"], client.authToken ? cp : null]
      .filter(Boolean)
      .join(",");
    try {
      return await client.beta.messages
        .create(body, {
          signal,
          headers: { ...headers, ...(beta ? { "anthropic-beta": beta } : {}) },
        })
        .asResponse();
    } catch (error) {
      // The SDK throws on HTTP failures; retain the provider's native error body
      // and rate-limit metadata, rather than turning every status into a 502.
      if (error.status && error.headers && error.error)
        return Response.json(error.error, {
          status: error.status,
          headers: Object.fromEntries(error.headers),
        });
      throw error;
    }
  },
);
server.listen(Number(process.env.CLAUDE_BRIDGE_PORT ?? 8091), "127.0.0.1", () => {
  clearInterval(startup);
  console.log(JSON.stringify({ port: server.address().port }));
});
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, () => {
    server.close(() => process.exit(0));
    server.closeAllConnections();
  });