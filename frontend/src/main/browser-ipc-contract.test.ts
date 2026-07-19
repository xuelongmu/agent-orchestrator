import { readFile } from "node:fs/promises";
import path from "node:path";

const browserInputTypes = ["BrowserBoundsInput", "BrowserNavigateInput"] as const;

test("browser IPC inputs have one authoritative declaration", async () => {
	const [hostSource, preloadSource] = await Promise.all([
		readFile(path.resolve("src/main/browser-view-host.ts"), "utf8"),
		readFile(path.resolve("src/preload.ts"), "utf8"),
	]);

	for (const typeName of browserInputTypes) {
		const declaration = new RegExp(`\\b(?:interface|type)\\s+${typeName}\\b`, "g");
		expect(hostSource.match(declaration), typeName).toHaveLength(1);
		expect(preloadSource, typeName).not.toMatch(declaration);
	}
});
