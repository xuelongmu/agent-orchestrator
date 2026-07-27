import { mkdirSync, readdirSync, renameSync, rmSync } from "node:fs";
import { basename, join } from "node:path";

/** Prefix for a daemon binary that had to be moved out of the way. */
export function asidePrefix(outPath) {
	return `${basename(outPath)}.aside-`;
}

/**
 * Clear the daemon build's output path without wiping the directory wholesale.
 *
 * Windows refuses to delete a loaded executable. The dev daemon from a previous
 * `npm run dev` outlives the supervisor by design (it drains, then self-stops),
 * so a directory wipe in the next launch's predev build aborts with EPERM before
 * Electron ever starts. Renaming a running image aside is permitted on Windows,
 * frees the canonical path for this build, and leaves the live daemon running.
 *
 * fs operations are injectable so the busy-executable branch is testable on
 * platforms that happily unlink a running binary.
 */
export function prepareOutDir(outDir, outPath, fs = {}) {
	const mkdir = fs.mkdir ?? ((dir) => mkdirSync(dir, { recursive: true }));
	const readdir = fs.readdir ?? readdirSync;
	const remove = fs.remove ?? ((path) => rmSync(path, { force: true }));
	const rename = fs.rename ?? renameSync;
	const pid = fs.pid ?? process.pid;
	const prefix = asidePrefix(outPath);

	mkdir(outDir);

	// Sweep binaries set aside by earlier runs. One still in use stays put and is
	// collected by a later build.
	for (const entry of readdir(outDir)) {
		if (!entry.startsWith(prefix)) continue;
		try {
			remove(join(outDir, entry));
		} catch {
			// Still loaded; a later build will get it.
		}
	}

	try {
		remove(outPath);
		return { setAside: null };
	} catch (error) {
		if (!["EPERM", "EACCES", "EBUSY", "ETXTBSY"].includes(error.code)) throw error;
		const setAside = join(outDir, `${prefix}${pid}`);
		rename(outPath, setAside);
		return { setAside };
	}
}
