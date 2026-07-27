import { describe, expect, it } from "vitest";
import { asideDir, prepareOutDir } from "./daemon-out-dir.mjs";

const OUT_DIR = "/repo/frontend/daemon";
const OUT_PATH = `${OUT_DIR}/ao.exe`;
const ASIDE_DIR = "/repo/frontend/.daemon-aside";

function errno(code) {
	return Object.assign(new Error(code), { code });
}

/** Records what prepareOutDir did, and can make specific paths undeletable. */
function fakeFs({ entries = [], busy = [] } = {}) {
	const calls = { mkdir: [], removed: [], renamed: [] };
	return {
		calls,
		ops: {
			pid: 4242,
			mkdir: (dir) => calls.mkdir.push(dir),
			readdir: () => entries,
			remove: (path) => {
				if (busy.includes(path)) throw errno("EPERM");
				calls.removed.push(path);
			},
			rename: (from, to) => calls.renamed.push([from, to]),
		},
	};
}

describe("asideDir", () => {
	// forge.config.ts ships the whole `daemon` directory via extraResource, and a
	// packaging build cannot sweep an aside it just created — parking inside the
	// output directory would ship a stale second executable.
	it("parks busy binaries outside the packaged resource directory", () => {
		expect(asideDir(OUT_DIR)).toBe(ASIDE_DIR);
		expect(asideDir(OUT_DIR).startsWith(`${OUT_DIR}/`)).toBe(false);
	});

	it("stays on the same volume as the output directory so the rename cannot EXDEV", () => {
		expect(asideDir(OUT_DIR)).toBe(`${OUT_DIR.slice(0, OUT_DIR.lastIndexOf("/"))}/.daemon-aside`);
	});
});

describe("prepareOutDir", () => {
	it("removes only the output binary, never the directory", () => {
		const fs = fakeFs();
		expect(prepareOutDir(OUT_DIR, OUT_PATH, fs.ops)).toEqual({ setAside: null });
		expect(fs.calls.mkdir).toEqual([OUT_DIR, ASIDE_DIR]);
		expect(fs.calls.removed).toEqual([OUT_PATH]);
		expect(fs.calls.renamed).toEqual([]);
	});

	// The Windows failure this exists for: quitting the dev harness leaves the
	// daemon draining, its ao.exe stays loaded, and the next `npm run dev` runs
	// predev before Electron attaches. Deleting would EPERM and abort the launch.
	it("moves a loaded executable aside instead of failing the build", () => {
		const fs = fakeFs({ busy: [OUT_PATH] });
		expect(prepareOutDir(OUT_DIR, OUT_PATH, fs.ops)).toEqual({
			setAside: `${ASIDE_DIR}/ao.exe.aside-4242`,
		});
		expect(fs.calls.renamed).toEqual([[OUT_PATH, `${ASIDE_DIR}/ao.exe.aside-4242`]]);
	});

	it("sweeps binaries parked by earlier runs and ignores ones still loaded", () => {
		const stale = `${ASIDE_DIR}/ao.exe.aside-1`;
		const stillLoaded = `${ASIDE_DIR}/ao.exe.aside-2`;
		const fs = fakeFs({
			entries: ["ao.exe.aside-1", "ao.exe.aside-2", "unrelated"],
			busy: [stillLoaded],
		});

		expect(() => prepareOutDir(OUT_DIR, OUT_PATH, fs.ops)).not.toThrow();
		expect(fs.calls.removed).toEqual([stale, OUT_PATH]);
	});

	it("rethrows failures that are not a busy executable", () => {
		const fs = fakeFs();
		fs.ops.remove = () => {
			throw errno("ENOSPC");
		};
		expect(() => prepareOutDir(OUT_DIR, OUT_PATH, fs.ops)).toThrow(/ENOSPC/);
	});
});
