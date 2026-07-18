import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { enqueueSCMWebhookDelivery, processSCMWebhookQueue } from "../scm-webhook-queue.js";
import type { SessionId } from "../types.js";

describe("durable SCM webhook queue", () => {
  let tempDir: string;
  let queuePath: string;
  let now: number;

  beforeEach(() => {
    tempDir = mkdtempSync(join(tmpdir(), "ao-webhook-queue-"));
    queuePath = join(tempDir, "webhook-events.json");
    now = 1_000;
  });

  afterEach(() => {
    rmSync(tempDir, { recursive: true, force: true });
  });

  it("deduplicates a delivery before and after successful processing", async () => {
    const input = {
      provider: "github",
      deliveryId: "delivery-123",
      projectId: "my-app",
      sessionIds: ["app-1" as SessionId],
    };

    const first = enqueueSCMWebhookDelivery(input, { queuePath, now: () => now });
    const duplicateWhilePending = enqueueSCMWebhookDelivery(input, {
      queuePath,
      now: () => now,
    });
    expect(first).toMatchObject({ duplicate: false, enqueuedJobs: 1 });
    expect(duplicateWhilePending).toMatchObject({ duplicate: true, enqueuedJobs: 0 });

    const processor = vi.fn(async () => {});
    const processed = await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
    });
    expect(processed.processed).toHaveLength(1);
    expect(processor).toHaveBeenCalledTimes(1);

    const duplicateAfterCompletion = enqueueSCMWebhookDelivery(input, {
      queuePath,
      now: () => now,
    });
    expect(duplicateAfterCompletion).toMatchObject({ duplicate: true, enqueuedJobs: 0 });
    expect(processor).toHaveBeenCalledTimes(1);
  });

  it("keeps transient failures durable and retries them after backoff", async () => {
    enqueueSCMWebhookDelivery(
      {
        provider: "github",
        deliveryId: "delivery-retry",
        projectId: "my-app",
        sessionIds: ["app-2" as SessionId],
      },
      { queuePath, now: () => now },
    );

    const processor = vi
      .fn<(job: { sessionId: SessionId }) => Promise<void>>()
      .mockRejectedValueOnce(new Error("temporary GitHub outage"))
      .mockResolvedValue(undefined);
    const failed = await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
      retryBaseMs: 1_000,
    });
    expect(failed.failures).toHaveLength(1);
    expect(processor).toHaveBeenCalledTimes(1);

    const persistedAfterFailure = JSON.parse(readFileSync(queuePath, "utf-8")) as {
      jobs: Array<{ attemptCount: number; availableAt: number }>;
    };
    expect(persistedAfterFailure.jobs).toEqual([
      expect.objectContaining({ attemptCount: 1, availableAt: 2_000 }),
    ]);

    now = 1_999;
    await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
      retryBaseMs: 1_000,
    });
    expect(processor).toHaveBeenCalledTimes(1);

    now = 2_000;
    const retried = await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
      retryBaseMs: 1_000,
    });
    expect(retried.processed).toHaveLength(1);
    expect(processor).toHaveBeenCalledTimes(2);
    expect(JSON.parse(readFileSync(queuePath, "utf-8"))).toMatchObject({ jobs: [] });
  });

  it("does not claim work newer than a lifecycle poll snapshot", async () => {
    enqueueSCMWebhookDelivery(
      {
        provider: "github",
        deliveryId: "delivery-race",
        projectId: "my-app",
        sessionIds: ["app-3" as SessionId],
      },
      { queuePath, now: () => now },
    );

    const processor = vi.fn(async () => {});
    await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
      enqueuedBefore: now,
    });
    expect(processor).not.toHaveBeenCalled();

    await processSCMWebhookQueue("my-app", processor, {
      queuePath,
      now: () => now,
      enqueuedBefore: now + 1,
    });
    expect(processor).toHaveBeenCalledTimes(1);
  });
});
