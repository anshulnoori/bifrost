// Generated names satisfy the provider-key uniqueness constraint but are not
// user aliases. Support both the original short suffix and stable full IDs.
export function chatgptAccountAlias(name: string): string {
	const alias = name.trim();
	return /^ChatGPT [\da-f]{8}(?:-[\da-f]{4}-[\da-f]{4}-[\da-f]{4}-[\da-f]{12})?$/i.test(alias) ? "" : alias;
}

export function chatgptAccountLabel(name: string, email?: string): string {
	const address = email?.trim();
	if (!address) return name;
	const alias = chatgptAccountAlias(name);
	return alias && alias.toLowerCase() !== address.toLowerCase() ? `${alias} (${address})` : address;
}