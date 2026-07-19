import { readFile } from "node:fs/promises";

const browserInputTypes = ["BrowserBoundsInput", "BrowserNavigateInput"] as const;

test("browser IPC inputs have one authoritative declaration", async () => {
	const [hostSource, preloadSource] = await Promise.all([
		readFile(new URL("./browser-view-host.ts", import.meta.url), "utf8"),
		readFile(new URL("../preload.ts", import.meta.url), "utf8"),
	]);

	for (const typeName of browserInputTypes) {
		const declaration = new RegExp(`\\b(?:interface|type)\\s+${typeName}\\b`, "g");
		expect(hostSource.match(declaration), typeName).toHaveLength(1);
		expect(preloadSource, typeName).not.toMatch(declaration);
	}
});
