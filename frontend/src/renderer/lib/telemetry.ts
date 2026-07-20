import posthog from "posthog-js/dist/module.full.no-external";
import { aoBridge } from "./bridge";
import { isLoopbackHostname } from "./loopback";
import { DEFAULT_POSTHOG_HOST, DEFAULT_POSTHOG_PROJECT_KEY } from "../../shared/posthog-config";

const POSTHOG_KEY = import.meta.env.VITE_AO_POSTHOG_KEY?.trim() || DEFAULT_POSTHOG_PROJECT_KEY;
const POSTHOG_HOST = import.meta.env.VITE_AO_POSTHOG_HOST?.trim() || DEFAULT_POSTHOG_HOST;
const RELEASE_TAG = "2026-01-30";
const REDACTED_LOCAL_URL = "[redacted-local-url]";
const REDACTED_LOCAL_PATH = "[redacted-local-path]";
const DAILY_ACTIVE_STORAGE_KEY = "ao.telemetry.lastActiveDate";
const CAPTURE_BUDGET_STORAGE_KEY = "ao.telemetry.captureBudget.v1";
const EMBEDDED_LOCAL_URL_PATTERN =
	/(?:\bfile:\/\/\/\S+|\bapp:\/\/renderer\/\S+|\bhttps?:\/\/(?:localhost|127\.0\.0\.1|\[::1\])(?::\d+)?\S*)/gi;

let initPromise: Promise<boolean> | null = null;
let errorHandlersBound = false;
let telemetryContext: TelemetryProperties = {};
let fallbackDailyActiveDate = "";

// Burst state is intentionally process-local. Daily counts use localStorage so
// a renderer reload cannot hand a crash/re-render loop a fresh daily budget.
const EVENTS_PER_NAME_PER_MINUTE = 5;
const EVENTS_PER_NAME_PER_DAY = 200;
const MINUTE_MS = 60_000;
const minuteWindows = new Map<string, { start: number; count: number }>();
const fallbackDayWindows = new Map<string, { day: string; count: number }>();

type TelemetryProperties = Record<string, unknown>;
type DailyActiveStorage = Pick<Storage, "getItem" | "setItem">;
type CaptureBudgetStorage = Pick<Storage, "getItem" | "setItem">;
type PostHogPersistenceStorage = Pick<Storage, "getItem" | "removeItem">;
type DailyActiveEventTarget = {
	addEventListener: (type: string, listener: EventListener, options?: AddEventListenerOptions) => void;
	removeEventListener: (type: string, listener: EventListener, options?: EventListenerOptions) => void;
};

export type DailyActiveHeartbeatOptions = {
	storage?: DailyActiveStorage;
	now?: () => Date;
	capture: () => void;
	window: DailyActiveEventTarget;
	document: DailyActiveEventTarget & Pick<Document, "visibilityState">;
};

export function buildTelemetryContext(appVersion: string, platform: string): TelemetryProperties {
	const version = appVersion.trim() || "unknown";
	return {
		app_version: version,
		ao_version: version,
		platform,
		build_mode: import.meta.env.DEV ? "dev" : "packaged",
	};
}

function browserLocalStorage(): Storage | undefined {
	try {
		return window.localStorage;
	} catch {
		return undefined;
	}
}

function reserveMinute(name: string, now: number): boolean {
	const window = minuteWindows.get(name);
	if (!window || now - window.start >= MINUTE_MS) {
		minuteWindows.set(name, { start: now, count: 1 });
		return true;
	}
	if (window.count >= EVENTS_PER_NAME_PER_MINUTE) return false;
	window.count += 1;
	return true;
}

function reserveFallbackDay(name: string, day: string): boolean {
	const window = fallbackDayWindows.get(name);
	if (!window || window.day !== day) {
		fallbackDayWindows.set(name, { day, count: 1 });
		return true;
	}
	if (window.count >= EVENTS_PER_NAME_PER_DAY) return false;
	window.count += 1;
	return true;
}

// reserveCapture enforces a process-local burst bound and, when localStorage is
// available, a reload-safe per-UTC-day budget for each renderer event name.
export function reserveCapture(
	name: string,
	now = Date.now(),
	storage: CaptureBudgetStorage | undefined = browserLocalStorage(),
): boolean {
	if (!reserveMinute(name, now)) return false;
	const day = new Date(now).toISOString().slice(0, 10);
	if (!storage) return reserveFallbackDay(name, day);
	try {
		const raw = storage.getItem(CAPTURE_BUDGET_STORAGE_KEY);
		const parsed = raw ? (JSON.parse(raw) as { day?: unknown; counts?: unknown }) : {};
		const counts =
			parsed.day === day && parsed.counts && typeof parsed.counts === "object"
				? { ...(parsed.counts as Record<string, unknown>) }
				: {};
		const storedCount = counts[name];
		const count = typeof storedCount === "number" && Number.isFinite(storedCount)
			? Math.max(0, Math.floor(storedCount))
			: 0;
		if (count >= EVENTS_PER_NAME_PER_DAY) return false;
		counts[name] = count + 1;
		storage.setItem(CAPTURE_BUDGET_STORAGE_KEY, JSON.stringify({ day, counts }));
		return true;
	} catch {
		return reserveFallbackDay(name, day);
	}
}

// migrateIdentifiedPostHogPersistence removes legacy identify() state before
// PostHog initializes. Anonymous persistence is retained, while malformed
// legacy state is removed conservatively so an upgrade cannot resume an
// identified person profile.
export function migrateIdentifiedPostHogPersistence(
	storage: PostHogPersistenceStorage | undefined,
	projectKey: string,
): boolean {
	if (!storage || !projectKey) return false;
	const key = `ph_${projectKey}_posthog`;
	const raw = storage.getItem(key);
	if (!raw) return false;
	try {
		const state = JSON.parse(raw) as Record<string, unknown>;
		if (state.$user_state !== "identified" && typeof state.$user_id !== "string") return false;
	} catch {
		// Unknown legacy persistence must not be allowed to restore identity.
	}
	storage.removeItem(key);
	return true;
}

function withTelemetryContext(properties: TelemetryProperties): TelemetryProperties {
	return { ...telemetryContext, ...properties };
}

export function reserveDailyActiveCapture(storage?: DailyActiveStorage, now = new Date()): boolean {
	const utcDate = now.toISOString().slice(0, 10);
	if (!storage) {
		if (fallbackDailyActiveDate === utcDate) return false;
		fallbackDailyActiveDate = utcDate;
		return true;
	}
	try {
		if (storage.getItem(DAILY_ACTIVE_STORAGE_KEY) === utcDate) return false;
		storage.setItem(DAILY_ACTIVE_STORAGE_KEY, utcDate);
		return true;
	} catch {
		if (fallbackDailyActiveDate === utcDate) return false;
		fallbackDailyActiveDate = utcDate;
		return true;
	}
}

export function startDailyActiveHeartbeat({
	storage,
	now = () => new Date(),
	capture,
	window,
	document,
}: DailyActiveHeartbeatOptions): () => void {
	const maybeCapture = () => {
		if (reserveDailyActiveCapture(storage, now())) {
			capture();
		}
	};
	const onVisibilityChange = () => {
		if (document.visibilityState === "visible") {
			maybeCapture();
		}
	};
	const activityEvents = ["pointerdown", "keydown"] as const;
	const passiveOptions = { passive: true };

	maybeCapture();
	window.addEventListener("focus", maybeCapture);
	document.addEventListener("visibilitychange", onVisibilityChange);
	for (const event of activityEvents) {
		document.addEventListener(event, maybeCapture, passiveOptions);
	}

	return () => {
		window.removeEventListener("focus", maybeCapture);
		document.removeEventListener("visibilitychange", onVisibilityChange);
		for (const event of activityEvents) {
			document.removeEventListener(event, maybeCapture);
		}
	};
}

function normalizeException(reason: unknown): Error {
	if (reason instanceof Error) return reason;
	if (typeof reason === "string") return new Error(reason);
	try {
		return new Error(JSON.stringify(reason));
	} catch {
		return new Error("Unknown renderer exception");
	}
}

function routeSurface(pathname: string): string {
	if (pathname === "/") return "home";
	if (/^\/prs(?:\/|$)/.test(pathname)) return "pull_requests";
	if (/^\/projects\/[^/]+\/sessions\/[^/]+$/.test(pathname)) return "session_detail";
	if (/^\/projects\/[^/]+(?:\/|$)/.test(pathname)) {
		if (/\/settings$/.test(pathname)) return "project_settings";
		return "project_board";
	}
	if (/^\/sessions\/[^/]+$/.test(pathname)) return "session_detail";
	return "other";
}

async function sha256Hex(raw: string): Promise<string> {
	const subtle = globalThis.crypto?.subtle;
	if (!subtle) return "redacted";
	const bytes = new TextEncoder().encode(raw);
	const digest = await subtle.digest("SHA-256", bytes);
	return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function hashedTelemetryID(value: unknown): Promise<string | undefined> {
	if (typeof value !== "string") return undefined;
	const trimmed = value.trim();
	if (!trimmed) return undefined;
	return sha256Hex(trimmed);
}

function isLocalURL(value: string): boolean {
	try {
		const url = new URL(value);
		return (
			url.protocol === "file:" ||
			(url.protocol === "app:" && url.host === "renderer") ||
			isLoopbackHostname(url.hostname)
		);
	} catch {
		return false;
	}
}

function redactEmbeddedLocalURLs(value: string): string {
	return value.replace(EMBEDDED_LOCAL_URL_PATTERN, REDACTED_LOCAL_URL);
}

function redactEmbeddedAbsolutePaths(value: string): string {
	return value
		.replace(/(?:\/Users\/|\/home\/|\/tmp\/|\/private\/var\/|\/var\/folders\/)\S+/g, REDACTED_LOCAL_PATH)
		.replace(/\b[A-Za-z]:\\[^\s)]+/g, REDACTED_LOCAL_PATH);
}

function sanitizeSensitiveString(value: string): string {
	const trimmed = value.trim();
	if (!trimmed) return trimmed;
	if (isLocalURL(trimmed)) return REDACTED_LOCAL_URL;
	return redactEmbeddedAbsolutePaths(redactEmbeddedLocalURLs(trimmed));
}

function sanitizePostHogValue(value: unknown): unknown {
	if (typeof value === "string") return sanitizeSensitiveString(value);
	if (Array.isArray(value)) return value.map((item) => sanitizePostHogValue(item));
	if (value && typeof value === "object") {
		return Object.fromEntries(Object.entries(value).map(([key, nested]) => [key, sanitizePostHogValue(nested)]));
	}
	return value;
}

export function sanitizePostHogEvent(event: Record<string, unknown>): Record<string, unknown> {
	return sanitizePostHogValue(event) as Record<string, unknown>;
}

export function sanitizeReplayRequestName(name: string): string {
	const withoutQuery = name.split("?")[0] ?? name;
	return sanitizeSensitiveString(withoutQuery);
}

function sanitizePostHogCaptureResult<T>(event: T): T {
	return sanitizePostHogEvent(event as unknown as Record<string, unknown>) as unknown as T;
}

async function sanitizeRendererContextProperties(properties?: TelemetryProperties): Promise<TelemetryProperties> {
	const safe: TelemetryProperties = {};
	if (typeof properties?.source === "string" && properties.source.trim() !== "") {
		safe.source = properties.source;
	}
	if (typeof properties?.operation === "string" && properties.operation.trim() !== "") {
		safe.operation = properties.operation;
	}
	if (typeof properties?.surface === "string" && properties.surface.trim() !== "") {
		safe.surface = properties.surface;
	}
	if (typeof properties?.unhandled === "boolean") {
		safe.unhandled = properties.unhandled;
	}
	const projectIDHash = await hashedTelemetryID(properties?.project_id);
	if (projectIDHash) {
		safe.project_id_hash = projectIDHash;
	}
	return safe;
}

// Allowed `source` enum for the orchestrator-spawn triad. Kept as a literal set
// here (rather than imported from spawn-orchestrator.ts, which imports this
// module) to avoid a cycle; keep in sync with OrchestratorSpawnSource.
const ORCHESTRATOR_SPAWN_SOURCES = new Set([
	"board",
	"restore_dialog",
	"topbar",
	"sidebar",
	"project_add",
	"settings",
	"restart",
]);

export async function sanitizeRendererProperties(
	event: string,
	properties?: TelemetryProperties,
): Promise<TelemetryProperties> {
	const safe: TelemetryProperties = {};
	switch (event) {
		case "ao.app.active":
			if (properties?.channel === "renderer") safe.channel = "renderer";
			break;
		case "ao.renderer.route_viewed":
			if (typeof properties?.surface === "string" && properties.surface.trim() !== "") {
				safe.surface = properties.surface;
			}
			break;
		case "ao.renderer.project_add_requested":
		case "ao.renderer.loaded":
			break;
		case "ao.renderer.project_add_succeeded":
		case "ao.renderer.project_removed":
		case "ao.renderer.orchestrator_open_requested":
		case "ao.renderer.task_create_requested":
		case "ao.renderer.task_create_succeeded":
		case "ao.renderer.task_create_failed":
		case "ao.renderer.session_kill_requested":
		case "ao.renderer.session_kill_succeeded":
		case "ao.renderer.session_kill_failed":
		case "ao.renderer.settings_save_requested":
		case "ao.renderer.settings_save_succeeded":
		case "ao.renderer.settings_save_failed": {
			const projectIDHash = await hashedTelemetryID(properties?.project_id);
			if (projectIDHash) safe.project_id_hash = projectIDHash;
			break;
		}
		case "ao.renderer.orchestrator_spawn_requested":
		case "ao.renderer.orchestrator_spawn_succeeded":
		case "ao.renderer.orchestrator_spawn_failed": {
			const projectIDHash = await hashedTelemetryID(properties?.project_id);
			if (projectIDHash) safe.project_id_hash = projectIDHash;
			if (typeof properties?.source === "string" && ORCHESTRATOR_SPAWN_SOURCES.has(properties.source)) {
				safe.source = properties.source;
			}
			break;
		}
		case "ao.renderer.notification_opened":
			if (properties?.target === "pr" || properties?.target === "session") safe.target = properties.target;
			break;
		case "ao.renderer.notification_mark_read_requested":
		case "ao.renderer.notification_mark_read_succeeded":
		case "ao.renderer.notification_mark_read_failed":
			if (properties?.scope === "single" || properties?.scope === "all") safe.scope = properties.scope;
			break;
		case "ao.renderer.daemon_failure":
			if (typeof properties?.daemon_state === "string") safe.daemon_state = properties.daemon_state;
			if (typeof properties?.code === "string") safe.code = properties.code;
			if (typeof properties?.exit_code === "number") safe.exit_code = properties.exit_code;
			if (typeof properties?.signal === "string") safe.signal = properties.signal;
			break;
		case "ao.renderer.api_error":
			if (typeof properties?.operation === "string") safe.operation = properties.operation;
			if (typeof properties?.error_category === "string") safe.error_category = properties.error_category;
			if (typeof properties?.status === "number") safe.status = properties.status;
			break;
		case "ao.renderer.terminal_attach_failed":
			if (properties?.reason === "open_timeout" || properties?.reason === "pane_error") {
				safe.reason = properties.reason;
			}
			break;
	}
	return safe;
}

function exceptionName(error: unknown): string {
	if (error instanceof Error && error.name.trim() !== "") return error.name.trim();
	if (typeof error === "string") return "string";
	return "unknown";
}

export async function sanitizeRendererExceptionProperties(
	error: unknown,
	properties?: TelemetryProperties,
): Promise<TelemetryProperties> {
	const safe: TelemetryProperties = {
		error_name: exceptionName(error),
	};
	return { ...safe, ...(await sanitizeRendererContextProperties(properties)) };
}

function bindErrorHandlers() {
	if (errorHandlersBound) return;
	errorHandlersBound = true;
	window.addEventListener("error", (event) => {
		void captureRendererException(event.error ?? new Error(event.message), {
			source: "window-error",
			unhandled: true,
		});
	});
	window.addEventListener("unhandledrejection", (event) => {
		void captureRendererException(normalizeException(event.reason), {
			source: "unhandledrejection",
			unhandled: true,
		});
	});
}

export async function initTelemetry(): Promise<boolean> {
	if (initPromise) return initPromise;
	initPromise = (async () => {
		if (!POSTHOG_KEY) return false;
		const bootstrap = await aoBridge.telemetry.getBootstrap();
		if (!bootstrap) return false;
		telemetryContext = buildTelemetryContext(bootstrap.appVersion, bootstrap.platform);
		const storage = browserLocalStorage();
		migrateIdentifiedPostHogPersistence(storage, POSTHOG_KEY);
		posthog.init(POSTHOG_KEY, {
			api_host: POSTHOG_HOST,
			defaults: RELEASE_TAG,
			autocapture: false,
			capture_pageview: false,
			capture_exceptions: false,
			persistence: "localStorage",
			person_profiles: "never",
			bootstrap: { distinctID: bootstrap.distinctId, isIdentifiedID: false },
			before_send: (event) => (event ? sanitizePostHogCaptureResult(event) : event),
			session_recording: {
				maskCapturedNetworkRequestFn: (request) => {
					if (request.name) {
						request.name = sanitizeReplayRequestName(request.name);
					}
					return request;
				},
			},
		});
		posthog.register({
			...telemetryContext,
			surface: "renderer",
		});
		bindErrorHandlers();
		startDailyActiveHeartbeat({
			storage,
			window,
			document,
			capture: () => {
				void (async () => {
					posthog.capture(
						"ao.app.active",
						withTelemetryContext(await sanitizeRendererProperties("ao.app.active", { channel: "renderer" })),
					);
				})();
			},
		});
		if (reserveCapture("ao.renderer.loaded", Date.now(), storage)) {
			posthog.capture("ao.renderer.loaded", withTelemetryContext(await sanitizeRendererProperties("ao.renderer.loaded")));
		}
		return true;
	})().catch(() => false);
	return initPromise;
}

export async function captureRendererEvent(event: string, properties?: Record<string, unknown>): Promise<void> {
	if (!(await initTelemetry())) return;
	if (!reserveCapture(event)) return;
	const safeProperties = withTelemetryContext(await sanitizeRendererProperties(event, properties));
	posthog.capture(event, safeProperties);
}

export async function captureRendererException(error: unknown, properties?: Record<string, unknown>): Promise<void> {
	if (!(await initTelemetry())) return;
	if (!reserveCapture(`exception:${exceptionName(error)}`)) return;
	const safeProperties = withTelemetryContext(await sanitizeRendererExceptionProperties(error, properties));
	posthog.captureException(normalizeException(error), safeProperties);
}

export async function addRendererExceptionStep(message: string, properties?: Record<string, unknown>): Promise<void> {
	if (!(await initTelemetry())) return;
	const safeProperties = withTelemetryContext(await sanitizeRendererContextProperties(properties));
	posthog.addExceptionStep(message, safeProperties);
}

export { routeSurface };
