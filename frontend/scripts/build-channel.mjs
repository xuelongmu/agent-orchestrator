const supportedChannels = new Set(["development", "testing", "nightly", "stable"]);

// buildChannel keeps release classification tied to the build context. A
// stable package.json is also present in ordinary checkouts, so only an
// explicit release-workflow value may classify a build as stable.
export function buildChannel(explicit, packageVersion) {
	const requested = explicit?.trim();
	if (requested) {
		if (!supportedChannels.has(requested)) {
			throw new Error(`unsupported AO_BUILD_CHANNEL: ${requested}`);
		}
		return requested;
	}
	if (packageVersion.includes("-nightly.")) return "nightly";
	if (packageVersion.includes("-testing")) return "testing";
	return "development";
}
