import { describe, expect, it } from "vitest";
import type { ObservabilitySummary } from "@aoagents/ao-core";
import { renderPrometheus } from "@/lib/metrics-prometheus";

type ProjectSnapshot = ObservabilitySummary["projects"][string];

const NOW = "2026-07-18T00:00:00.000Z";

function makeProject(overrides: Partial<ProjectSnapshot> = {}): ProjectSnapshot {
  return {
    projectId: "project-a",
    updatedAt: NOW,
    metrics: {},
    health: {},
    recentTraces: [],
    sessions: {},
    ...overrides,
  };
}

function makeSummary(
  projects: ObservabilitySummary["projects"] = {},
  overallStatus: ObservabilitySummary["overallStatus"] = "ok",
): ObservabilitySummary {
  return { generatedAt: NOW, overallStatus, projects };
}

function makeTrace(operation: string, durationMs?: number) {
  return {
    id: `${operation}-${String(durationMs)}`,
    timestamp: NOW,
    component: "web-api",
    operation,
    outcome: "success" as const,
    correlationId: "api-test",
    durationMs,
  };
}

describe("renderPrometheus", () => {
  it("maps counters to success and failure series without a total outcome", () => {
    const body = renderPrometheus(
      makeSummary({
        "project-a": makeProject({
          metrics: {
            api_request: { total: 7, success: 5, failure: 2 },
            spawn: { total: 3, success: 2, failure: 1 },
          },
        }),
      }),
    );

    expect(body).toContain(
      'ao_operations_total{project="project-a",metric="api_request",outcome="success"} 5',
    );
    expect(body).toContain(
      'ao_operations_total{project="project-a",metric="api_request",outcome="failure"} 2',
    );
    expect(body).not.toContain('outcome="total"');

    const apiRequestValues = body
      .split("\n")
      .filter((line) => line.includes('metric="api_request"'))
      .map((line) => Number(line.slice(line.lastIndexOf(" ") + 1)));
    expect(apiRequestValues).toEqual([5, 2]);
    expect(apiRequestValues.reduce((sum, value) => sum + value, 0)).toBe(7);
  });

  it("escapes free-form project and operation label values", () => {
    const projectId = 'project"one\\two\nline';
    const operation = 'GET /api/"observability"\\check\nnext';
    const body = renderPrometheus(
      makeSummary({
        [projectId]: makeProject({
          projectId,
          recentTraces: [makeTrace(operation, 12)],
        }),
      }),
    );

    expect(body).toContain('project="project\\"one\\\\two\\nline"');
    expect(body).toContain('operation="GET /api/\\"observability\\"\\\\check\\nnext"');

    const sampleLine =
      /^[a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[a-zA-Z_][a-zA-Z0-9_]*="(?:\\.|[^"\\\n])*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:\\.|[^"\\\n])*")*\})? (?:[-+]?(?:\d+(?:\.\d+)?|\.\d+)(?:[eE][-+]?\d+)?|NaN|[+-]Inf)$/;
    for (const line of body.split("\n").filter((line) => line && !line.startsWith("#"))) {
      expect(line).toMatch(sampleLine);
    }
  });

  it("renders cumulative duration buckets with +Inf equal to count", () => {
    const operation = "GET /api/observability";
    const body = renderPrometheus(
      makeSummary({
        "project-a": makeProject({
          recentTraces: [1, 5, 6, 10_000, 10_001].map((duration) => makeTrace(operation, duration)),
        }),
      }),
    );
    const bucketLines = body
      .split("\n")
      .filter((line) =>
        line.startsWith(
          'ao_operation_duration_ms_bucket{project="project-a",operation="GET /api/observability"',
        ),
      );
    const bucketCounts = bucketLines.map((line) => Number(line.slice(line.lastIndexOf(" ") + 1)));

    expect(bucketCounts).toHaveLength(12);
    expect(
      bucketCounts.every((count, index) => index === 0 || count >= bucketCounts[index - 1]!),
    ).toBe(true);
    expect(bucketLines.at(-1)).toContain('le="+Inf"');
    expect(bucketCounts.at(-1)).toBe(5);
    expect(body).toContain(
      'ao_operation_duration_ms_sum{project="project-a",operation="GET /api/observability"} 20013',
    );
    expect(body).toContain(
      'ao_operation_duration_ms_count{project="project-a",operation="GET /api/observability"} 5',
    );
  });

  it("types sliding-window duration series as gauges when their values decrease", () => {
    const operation = "GET /api/observability";
    const before = renderPrometheus(
      makeSummary({
        "project-a": makeProject({
          recentTraces: [makeTrace(operation, 10), makeTrace(operation, 20)],
        }),
      }),
    );
    const after = renderPrometheus(
      makeSummary({
        "project-a": makeProject({ recentTraces: [makeTrace(operation, 5)] }),
      }),
    );

    expect(before).toContain(
      'ao_operation_duration_ms_bucket{project="project-a",operation="GET /api/observability",le="25"} 2',
    );
    expect(after).toContain(
      'ao_operation_duration_ms_bucket{project="project-a",operation="GET /api/observability",le="25"} 1',
    );
    expect(before).toContain(
      'ao_operation_duration_ms_sum{project="project-a",operation="GET /api/observability"} 30',
    );
    expect(after).toContain(
      'ao_operation_duration_ms_sum{project="project-a",operation="GET /api/observability"} 5',
    );
    expect(before).toContain(
      'ao_operation_duration_ms_count{project="project-a",operation="GET /api/observability"} 2',
    );
    expect(after).toContain(
      'ao_operation_duration_ms_count{project="project-a",operation="GET /api/observability"} 1',
    );
    for (const body of [before, after]) {
      expect(body).toContain("# TYPE ao_operation_duration_ms_bucket gauge");
      expect(body).toContain("# TYPE ao_operation_duration_ms_sum gauge");
      expect(body).toContain("# TYPE ao_operation_duration_ms_count gauge");
      expect(body).not.toContain("# TYPE ao_operation_duration_ms histogram");
    }
  });

  it("maps overall and per-surface health statuses to numeric gauges", () => {
    const body = renderPrometheus(
      makeSummary(
        {
          "project-a": makeProject({
            health: {
              api: {
                surface: "api",
                status: "ok",
                updatedAt: NOW,
                component: "web-api",
              },
              lifecycle: {
                surface: "lifecycle",
                status: "warn",
                updatedAt: NOW,
                component: "core",
              },
              runtime: {
                surface: "runtime",
                status: "error",
                updatedAt: NOW,
                component: "core",
              },
            },
          }),
        },
        "error",
      ),
    );

    expect(body).toContain("ao_overall_status 2");
    expect(body).toContain('ao_health_status{project="project-a",surface="api"} 0');
    expect(body).toContain('ao_health_status{project="project-a",surface="lifecycle"} 1');
    expect(body).toContain('ao_health_status{project="project-a",surface="runtime"} 2');
  });

  it("renders valid metadata and no project series for an empty summary", () => {
    const body = renderPrometheus(makeSummary());

    for (const family of [
      "ao_operations_total",
      "ao_overall_status",
      "ao_health_status",
      "ao_operation_duration_ms_bucket",
      "ao_operation_duration_ms_sum",
      "ao_operation_duration_ms_count",
    ]) {
      expect(body.match(new RegExp(`^# HELP ${family} `, "gm"))).toHaveLength(1);
      expect(body.match(new RegExp(`^# TYPE ${family} `, "gm"))).toHaveLength(1);
    }
    expect(body.split("\n").filter((line) => line && !line.startsWith("#"))).toEqual([
      "ao_overall_status 0",
    ]);
  });
});
