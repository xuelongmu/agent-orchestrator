import { realpathSync } from "node:fs";
import path from "node:path";

export type PathIdentityOptions = {
	platform?: NodeJS.Platform;
	realpath?: (value: string) => string;
};

function pathOperations(platform: NodeJS.Platform) {
	return platform === "win32" ? path.win32 : path.posix;
}

function isMissingPathError(error: unknown): boolean {
	if (typeof error !== "object" || error === null) return false;
	const code = (error as NodeJS.ErrnoException).code;
	return code === "ENOENT" || code === "ENOTDIR";
}

function canonicalPath(value: string, options: PathIdentityOptions): string {
	const platform = options.platform ?? process.platform;
	const paths = pathOperations(platform);
	const realpath = options.realpath ?? realpathSync.native;
	const resolved = paths.resolve(value);
	const missingSuffix: string[] = [];
	let current = resolved;

	for (;;) {
		try {
			return paths.join(realpath(current), ...missingSuffix);
		} catch (error) {
			if (!isMissingPathError(error)) return resolved;
			const parent = paths.dirname(current);
			if (parent === current) return resolved;
			missingSuffix.unshift(paths.basename(current));
			current = parent;
		}
	}
}

export function pathIdentityKey(value: string, options: PathIdentityOptions = {}): string {
	const platform = options.platform ?? process.platform;
	const canonical = canonicalPath(value, options);
	return platform === "win32" ? canonical.toLowerCase() : canonical;
}

export function samePath(a: string, b: string, options: PathIdentityOptions = {}): boolean {
	return pathIdentityKey(a, options) === pathIdentityKey(b, options);
}

export function pathInside(child: string, parent: string, options: PathIdentityOptions = {}): boolean {
	const platform = options.platform ?? process.platform;
	const paths = pathOperations(platform);
	const relative = paths.relative(pathIdentityKey(parent, options), pathIdentityKey(child, options));
	return (
		relative === "" || (relative !== ".." && !relative.startsWith(`..${paths.sep}`) && !paths.isAbsolute(relative))
	);
}
