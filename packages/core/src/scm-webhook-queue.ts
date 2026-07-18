import { randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { atomicWriteFileSync } from "./atomic-write.js";
import { withFileLockSync } from "./file-lock.js";
import { getProjectDir } from "./paths.js";
import type { SessionId } from "./types.js";

const WEBHOOK_QUEUE_VERSION = 1;
const DEFAULT_RETRY_BASE_MS = 1_000;
const MAX_RETRY_DELAY_MS = 60_000;
const DEFAULT_LEASE_MS = 10 * 60_000;

interface WebhookDeliveryRecord {
  provider: string;
  deliveryId?: string;
  receivedAt: number;
  completedAt?: number;
}

interface WebhookJobLease {
  ownerId: string;
  ownerPid: number;
  expiresAt: number;
}

export interface SCMWebhookQueueJob {
  id: string;
  deliveryKey: string;
  projectId: string;
  sessionId: SessionId;
  attemptCount: number;
  enqueuedAt: number;
  availableAt: number;
  lease?: WebhookJobLease;
}

interface WebhookQueueStore {
  version: typeof WEBHOOK_QUEUE_VERSION;
  deliveries: Record<string, WebhookDeliveryRecord>;
  jobs: SCMWebhookQueueJob[];
}

export interface EnqueueSCMWebhookDeliveryInput {
  provider: string;
  deliveryId?: string;
  projectId: string;
  sessionIds: SessionId[];
}

export interface EnqueueSCMWebhookDeliveryResult {
  deliveryKey: string;
  duplicate: boolean;
  enqueuedJobs: number;
}

export interface ProcessSCMWebhookQueueFailure {
  job: SCMWebhookQueueJob;
  error: unknown;
}

export interface ProcessSCMWebhookQueueResult {
  processed: SCMWebhookQueueJob[];
  failures: ProcessSCMWebhookQueueFailure[];
}

interface SCMWebhookQueueOptions {
  /** Restrict processing to newly-enqueued deliveries. */
  deliveryKeys?: ReadonlySet<string>;
  /** Do not acknowledge work that arrived after the caller's state snapshot. */
  enqueuedBefore?: number;
  /** @internal Test-only override. */
  queuePath?: string;
  /** @internal Test-only clock override. */
  now?: () => number;
  /** @internal Test-only retry override. */
  retryBaseMs?: number;
  /** @internal Test-only lease override. */
  leaseMs?: number;
}

/** Durable queue path for webhook-triggered lifecycle work. */
function getSCMWebhookQueuePath(projectId: string): string {
  return join(getProjectDir(projectId), "webhook-events.json");
}

function emptyWebhookQueue(): WebhookQueueStore {
  return { version: WEBHOOK_QUEUE_VERSION, deliveries: {}, jobs: [] };
}

function readWebhookQueue(path: string): WebhookQueueStore {
  if (!existsSync(path)) return emptyWebhookQueue();

  const parsed: unknown = JSON.parse(readFileSync(path, "utf-8"));
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`Invalid SCM webhook queue at ${path}`);
  }

  const candidate = parsed as Partial<WebhookQueueStore>;
  if (
    candidate.version !== WEBHOOK_QUEUE_VERSION ||
    !candidate.deliveries ||
    typeof candidate.deliveries !== "object" ||
    Array.isArray(candidate.deliveries) ||
    !Array.isArray(candidate.jobs)
  ) {
    throw new Error(`Invalid SCM webhook queue at ${path}`);
  }
  return candidate as WebhookQueueStore;
}

function writeWebhookQueue(path: string, store: WebhookQueueStore): void {
  mkdirSync(dirname(path), { recursive: true });
  atomicWriteFileSync(path, `${JSON.stringify(store, null, 2)}\n`);
}

function withWebhookQueue<T>(path: string, update: (store: WebhookQueueStore) => T): T {
  return withFileLockSync(`${path}.lock`, () => update(readWebhookQueue(path)));
}

function makeDeliveryKey(provider: string, deliveryId: string | undefined): string {
  return deliveryId ? `${provider}:${deliveryId}` : `${provider}:unkeyed:${randomUUID()}`;
}

/**
 * Persist lifecycle work before acknowledging a verified webhook delivery.
 * A delivery ID is recorded before any handler runs, so GitHub redeliveries do
 * not enqueue or execute the same work twice.
 */
export function enqueueSCMWebhookDelivery(
  input: EnqueueSCMWebhookDeliveryInput,
  options: Pick<SCMWebhookQueueOptions, "queuePath" | "now"> = {},
): EnqueueSCMWebhookDeliveryResult {
  const path = options.queuePath ?? getSCMWebhookQueuePath(input.projectId);
  const now = options.now?.() ?? Date.now();
  const deliveryKey = makeDeliveryKey(input.provider, input.deliveryId);

  return withWebhookQueue(path, (store) => {
    if (input.deliveryId && store.deliveries[deliveryKey]) {
      return { deliveryKey, duplicate: true, enqueuedJobs: 0 };
    }

    const sessionIds = [...new Set(input.sessionIds)];
    store.deliveries[deliveryKey] = {
      provider: input.provider,
      deliveryId: input.deliveryId,
      receivedAt: now,
      ...(sessionIds.length === 0 ? { completedAt: now } : {}),
    };
    for (const sessionId of sessionIds) {
      store.jobs.push({
        id: `${deliveryKey}:${sessionId}`,
        deliveryKey,
        projectId: input.projectId,
        sessionId,
        attemptCount: 0,
        enqueuedAt: now,
        availableAt: now,
      });
    }
    writeWebhookQueue(path, store);
    return { deliveryKey, duplicate: false, enqueuedJobs: sessionIds.length };
  });
}

function isProcessAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    // Windows can report EPERM for a live process in another security context.
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}

function canClaimJob(job: SCMWebhookQueueJob, now: number): boolean {
  if (job.availableAt > now) return false;
  if (!job.lease) return true;
  if (job.lease.expiresAt <= now) return true;
  return !isProcessAlive(job.lease.ownerPid);
}

function claimNextWebhookJob(
  path: string,
  projectId: string,
  ownerId: string,
  options: SCMWebhookQueueOptions,
): SCMWebhookQueueJob | null {
  return withWebhookQueue(path, (store) => {
    const now = options.now?.() ?? Date.now();
    const job = store.jobs.find(
      (candidate) =>
        candidate.projectId === projectId &&
        (!options.deliveryKeys || options.deliveryKeys.has(candidate.deliveryKey)) &&
        (options.enqueuedBefore === undefined ||
          candidate.enqueuedAt < options.enqueuedBefore) &&
        canClaimJob(candidate, now),
    );
    if (!job) return null;

    job.lease = {
      ownerId,
      ownerPid: process.pid,
      expiresAt: now + (options.leaseMs ?? DEFAULT_LEASE_MS),
    };
    writeWebhookQueue(path, store);
    return { ...job, lease: { ...job.lease } };
  });
}

function completeWebhookJob(
  path: string,
  job: SCMWebhookQueueJob,
  ownerId: string,
  now: number,
): void {
  withWebhookQueue(path, (store) => {
    const index = store.jobs.findIndex((candidate) => candidate.id === job.id);
    if (index === -1 || store.jobs[index]?.lease?.ownerId !== ownerId) return;

    store.jobs.splice(index, 1);
    if (!store.jobs.some((candidate) => candidate.deliveryKey === job.deliveryKey)) {
      const delivery = store.deliveries[job.deliveryKey];
      if (delivery) delivery.completedAt = now;
    }
    writeWebhookQueue(path, store);
  });
}

function retryWebhookJob(
  path: string,
  job: SCMWebhookQueueJob,
  ownerId: string,
  now: number,
  retryBaseMs: number,
): void {
  withWebhookQueue(path, (store) => {
    const queued = store.jobs.find((candidate) => candidate.id === job.id);
    if (!queued || queued.lease?.ownerId !== ownerId) return;

    queued.attemptCount += 1;
    const delay = Math.min(
      retryBaseMs * 2 ** Math.max(0, queued.attemptCount - 1),
      MAX_RETRY_DELAY_MS,
    );
    queued.availableAt = now + delay;
    delete queued.lease;
    writeWebhookQueue(path, store);
  });
}

/**
 * Claim and process due webhook jobs. Failures remain durable with exponential
 * retry; a crashed worker's lease is reclaimable as soon as its PID exits.
 */
export async function processSCMWebhookQueue(
  projectId: string,
  processor: (job: SCMWebhookQueueJob) => Promise<void>,
  options: SCMWebhookQueueOptions = {},
): Promise<ProcessSCMWebhookQueueResult> {
  const path = options.queuePath ?? getSCMWebhookQueuePath(projectId);
  if (!existsSync(path)) return { processed: [], failures: [] };

  const ownerId = randomUUID();
  const processed: SCMWebhookQueueJob[] = [];
  const failures: ProcessSCMWebhookQueueFailure[] = [];

  while (true) {
    const job = claimNextWebhookJob(path, projectId, ownerId, options);
    if (!job) break;

    try {
      await processor(job);
      completeWebhookJob(path, job, ownerId, options.now?.() ?? Date.now());
      processed.push(job);
    } catch (error) {
      retryWebhookJob(
        path,
        job,
        ownerId,
        options.now?.() ?? Date.now(),
        options.retryBaseMs ?? DEFAULT_RETRY_BASE_MS,
      );
      failures.push({ job, error });
    }
  }

  return { processed, failures };
}
