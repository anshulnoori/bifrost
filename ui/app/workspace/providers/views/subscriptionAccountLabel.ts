export type SubscriptionProvider = "codex" | "chatgpt" | "claude";

// Generated names satisfy the provider-key uniqueness constraint but are not
// user aliases. Support both the original short suffix and stable full IDs.
export function subscriptionAccountAlias(name: string, provider: SubscriptionProvider): string {
	const alias = name.trim();
	const generatedName = `${provider} `;
	if (!alias.toLowerCase().startsWith(generatedName)) return alias;
	const suffix = alias.slice(generatedName.length);
	return /^[\da-f]{8}(?:-[\da-f]{4}-[\da-f]{4}-[\da-f]{4}-[\da-f]{12})?$/i.test(suffix) ? "" : alias;
}

export function subscriptionAccountLabel(name: string, email: string | undefined, provider: SubscriptionProvider): string {
	const address = email?.trim();
	if (!address) return name;
	const alias = subscriptionAccountAlias(name, provider);
	return alias && alias.toLowerCase() !== address.toLowerCase() ? `${alias} (${address})` : address;
}