function trimGitSuffix(pathname) {
	return pathname
		.replace(/^\/+/, "")
		.replace(/\.git$/, "")
		.replace(/\/+$/, "");
}

// repositoryIdentity returns a credential-free host/path identity suitable for
// build provenance. Unrecognized local paths and malformed remotes are omitted
// instead of being copied into the binary.
export function repositoryIdentity(remote) {
	const value = remote.trim();
	if (!value) return "";

	try {
		const url = new URL(value);
		if (!url.hostname) return "";
		const path = trimGitSuffix(url.pathname);
		return path ? `${url.hostname}/${path}` : "";
	} catch {
		const scp = /^(?:[^@/:]+@)?([^/:]+):(.+)$/.exec(value);
		if (!scp) return "";
		const path = trimGitSuffix(scp[2]);
		return path ? `${scp[1]}/${path}` : "";
	}
}
