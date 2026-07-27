import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { buildChannel } from "./build-channel.mjs";
import { prepareOutDir } from "./daemon-out-dir.mjs";
import { repositoryIdentity } from "./repository-identity.mjs";

const scriptsDir = dirname(fileURLToPath(import.meta.url));
const frontendRoot = resolve(scriptsDir, "..");
const repoRoot = resolve(frontendRoot, "..");
const backendRoot = join(repoRoot, "backend");
const outDir = join(frontendRoot, "daemon");
const outPath = join(outDir, process.platform === "win32" ? "ao.exe" : "ao");
const packageVersion = JSON.parse(readFileSync(join(frontendRoot, "package.json"), "utf8")).version;

// `--dev` builds the binary the dev harness (predev) spawns. It must keep the
// unstamped "dev" version that `go run`/`go build` produce, because the daemon
// pins each spawned session's PATH to its own directory (HookPATH): a stamped
// version makes the session's `ao` fail isDevBuild(), so `ao start` from inside
// a source checkout would open the released app instead of this checkout.
const devBuild = process.argv.includes("--dev");
const version = devBuild ? "dev" : packageVersion;

function gitOutput(...args) {
	const result = spawnSync("git", args, { cwd: repoRoot, encoding: "utf8" });
	return result.status === 0 ? result.stdout.trim() : "";
}

function repositoryName() {
	if (process.env.GITHUB_REPOSITORY) return process.env.GITHUB_REPOSITORY;
	const remote = gitOutput("config", "--get", "remote.origin.url");
	return repositoryIdentity(remote);
}

const commit = process.env.GITHUB_SHA || gitOutput("rev-parse", "HEAD");
const repository = repositoryName();
const builtAt = process.env.SOURCE_DATE_EPOCH
	? new Date(Number(process.env.SOURCE_DATE_EPOCH) * 1000).toISOString()
	: new Date().toISOString();
const channel = buildChannel(process.env.AO_BUILD_CHANNEL, packageVersion);
const cliPackage = "github.com/aoagents/agent-orchestrator/backend/internal/cli";
const ldflags = [
	`-X ${cliPackage}.Version=${version}`,
	`-X ${cliPackage}.Commit=${commit}`,
	`-X ${cliPackage}.Date=${builtAt}`,
	`-X ${cliPackage}.Repository=${repository}`,
	`-X ${cliPackage}.Channel=${channel}`,
].join(" ");

const { setAside } = prepareOutDir(outDir, outPath);
if (setAside) {
	console.log(`daemon binary in use; moved aside to ${setAside}`);
}

const result = spawnSync("go", ["build", "-trimpath", "-ldflags", ldflags, "-o", outPath, "./cmd/ao"], {
	cwd: backendRoot,
	stdio: "inherit",
});

if (result.error) {
	console.error(`failed to start go build: ${result.error.message}`);
	process.exit(1);
}

if (result.status !== 0) {
	process.exit(result.status ?? 1);
}
