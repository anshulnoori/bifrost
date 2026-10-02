// Generated names satisfy the provider-key uniqueness constraint but are not
// user aliases. Support both the original short suffix and stable full IDs.
export function codexAccountAlias(name: string, provider = "codex"): string {
	const alias = name.trim();
	const generated = /^(Codex|Claude) [\da-f]{8}(?:-[\da-f]{4}-[\da-f]{4}-[\da-f]{4}-[\da-f]{12})?$/i.exec(alias);
	return generated?.[1].toLowerCase() === provider ? "" : alias;
}

export function codexAccountLabel(name: string, email?: string, provider = "codex"): string {
	const address = email?.trim();
	if (!address) return name;
	const alias = codexAccountAlias(name, provider);
	return alias && alias.toLowerCase() !== address.toLowerCase() ? `${alias} (${address})` : address;
}