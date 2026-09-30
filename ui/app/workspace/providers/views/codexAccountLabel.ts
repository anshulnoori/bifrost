// Generated names satisfy the provider-key uniqueness constraint but are not
// user aliases. Support both the original short suffix and stable full IDs.
export function codexAccountAlias(name: string): string {
	const alias = name.trim();
	return /^Codex [\da-f]{8}(?:-[\da-f]{4}-[\da-f]{4}-[\da-f]{4}-[\da-f]{12})?$/i.test(alias) ? "" : alias;
}

export function codexAccountLabel(name: string, email?: string): string {
	const address = email?.trim();
	if (!address) return name;
	const alias = codexAccountAlias(name);
	return alias && alias.toLowerCase() !== address.toLowerCase() ? `${alias} (${address})` : address;
}