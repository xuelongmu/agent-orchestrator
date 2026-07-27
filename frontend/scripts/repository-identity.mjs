function trimGitSuffix(pathname) {
	return pathname
		.replace(/^\/+/, "")
		.replace(/\/+$/, "")
		.replace(/\.git$/, "");
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
		if (!path) return "";
		const identity = url.hostname.toLowerCase() === "github.com" ? path : `${url.hostname}/${path}`;
		return /\s/.test(identity) ? "" : identity;
	} catch {
		const scp = /^(?:[^@/:]+@)?([^/:]+):(.+)$/.exec(value);
		if (!scp) return "";
		const path = trimGitSuffix(scp[2]);
		if (!path) return "";
		const identity = scp[1].toLowerCase() === "github.com" ? path : `${scp[1]}/${path}`;
		return /\s/.test(identity) ? "" : identity;
	}
}
