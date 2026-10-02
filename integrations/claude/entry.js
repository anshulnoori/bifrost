// Build-specific imports must remain dynamic: Bun's static import table belongs
// to the original entry. Login and credential refresh use the original client.
if (!process.env.CLAUDE_CONFIG_DIR) throw new Error("set an owner-specific CLAUDE_CONFIG_DIR");
const startup = setInterval(() => {}, 1000);
// __BIFROST_BRIDGE__
// __BIFROST_ACCOUNTS__
const accounts = process.env.CLAUDE_BRIDGE_WORKER === "1" ? null : createAccounts(process.env.CLAUDE_CONFIG_DIR, process.execPath);
let ready;
let native;
async function initialize() {
  if (!ready) ready = (async () => {
    const { tD } = await import("/$bunfs/root/chunk-da9jta6b.js");
    const { startMdmRawRead } = await import("/$bunfs/root/chunk-cd7krv4h.js");
    const { startKeychainPrefetch } = await import("/$bunfs/root/chunk-y8e2dq66.js");
    startMdmRawRead();
    startKeychainPrefetch();
    const { enableConfigs } = await import("/$bunfs/root/chunk-ktfrs76h.js");
    await enableConfigs();
    const { cp } = await import("/$bunfs/root/chunk-djntk4j0.js");
    const { GL } = await import("/$bunfs/root/chunk-vrng99ca.js");
    const { LHe } = await import("/$bunfs/root/chunk-ydbv64xy.js");
    const { el, In, h$, JU } = await import("/$bunfs/root/chunk-k985080f.js");
    native = { tD, cp, GL, LHe, el, In, policy: h$, subscriptionScopes: JU };
  })();
  await ready;
}
let login;
let flow;
let expiry;
let starting;
async function status() {
  if (login && login.state !== "connected") return login;
  await initialize();
  const tokens = await native.el();
  if (!tokens?.accessToken) return { state: "disconnected" };
  if (!native.subscriptionScopes(tokens.scopes) || !(await native.policy()).valid) return { state: "reconnect_required" };
  return { state: "connected", email: native.In()?.emailAddress };
}
async function manage(method, path, body) {
  const route = /^\/accounts\/[a-zA-Z0-9_-]{1,128}(\/start|\/code)?$/.exec(path);
  if (!route) return Response.json({ error: "not found" }, { status: 404 });
  if (method === "GET" && !route[1]) return Response.json(await status());
  if (method !== "POST") return Response.json({ error: "not found" }, { status: 404 });
  await initialize();
  if (route[1] === "/code") {
    if (!flow || login?.state !== "pending" || body.id !== login.id)
      return Response.json({ error: "no pending login" }, { status: 409 });
    const parts = typeof body.code === "string" ? body.code.trim().split("#") : [];
    const expected = new URL(login.authorization_url).searchParams.get("state");
    if (parts.length !== 2 || !parts[0] || parts[1] !== expected)
      return Response.json({ error: "invalid authorization code or state" }, { status: 400 });
    // The native manual resolver ignores its state argument. Validate above
    // before handing off; the native loopback callback validates independently.
    flow.handleManualAuthCodeInput({ authorizationCode: parts[0], state: parts[1] });
    login = { ...login, state: "connecting" };
    clearTimeout(expiry);
    return Response.json(login);
  }
  if (route[1] !== "/start") return Response.json({ error: "not found" }, { status: 404 });
  if (starting) return Response.json(await starting);
  if (login && ["pending", "connecting"].includes(login.state)) return Response.json(login);
  flow?.cleanup();
  clearTimeout(expiry);
  const attempt = new native.GL();
  flow = attempt;
  const id = import.meta.require("node:crypto").randomUUID();
  let announce, failed;
  const announced = new Promise((resolve, reject) => { announce = resolve; failed = reject; });
  starting = announced;
  announced.then(() => { starting = null; }, () => { starting = null; });
  attempt.startOAuthFlow(async (manualURL) => {
    login = { id, state: "pending", authorization_url: manualURL, interval_seconds: 2 };
    announce(login);
  }, { loginWithClaudeAi: true, skipBrowserOpen: true }).then(async (tokens) => {
    if (flow !== attempt) return;
    if (!native.subscriptionScopes(tokens.scopes)) throw new Error("subscription login required");
    await native.LHe(tokens);
    const policy = await native.policy();
    if (!policy.valid) throw new Error("native login policy denied");
    if (flow === attempt) { login = { state: "connected" }; clearTimeout(expiry); }
  }).catch(() => {
    if (flow === attempt) { login = { state: "reconnect_required" }; clearTimeout(expiry); }
    failed(new Error("Claude login failed"));
  });
  expiry = setTimeout(() => {
    if (flow === attempt) { flow = null; attempt.cleanup(); login = { state: "expired" }; }
  }, 10 * 60 * 1000);
  return Response.json(await announced);
}
const server = createBridge(
  { token: process.env.CLAUDE_BRIDGE_TOKEN ?? "", manage: accounts?.manage ?? manage, requireAccount: !!accounts },
  accounts?.run ?? (async ({ body, headers, signal }) => {
    await initialize();
    if (!(await native.policy()).valid) throw new Error("native login policy denied");
    const client = await native.tD({
      maxRetries: 0,
      model: body.model,
      source: "bifrost",
      agentContext: { agentType: "main", isBackgroundAgent: false },
      signal,
    });
    const beta = [headers["anthropic-beta"], client.authToken ? native.cp : null]
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
  }),
);
server.listen(Number(process.env.CLAUDE_BRIDGE_PORT ?? 8091), "127.0.0.1", () => {
  clearInterval(startup);
  console.log(JSON.stringify({ port: server.address().port }));
});
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, async () => {
    flow?.cleanup();
    clearTimeout(expiry);
    await accounts?.close();
    server.close(() => process.exit(0));
    server.closeAllConnections();
  });
