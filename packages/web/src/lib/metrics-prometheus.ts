import type { ObservabilitySummary } from "@aoagents/ao-core";

const DURATION_BUCKETS_MS = [5, 10, 25, 50, 100, 250, 500, 1_000, 2_500, 5_000, 10_000];

const STATUS_VALUE: Record<ObservabilitySummary["overallStatus"], number> = {
  ok: 0,
  warn: 1,
  error: 2,
};

function escapeLabelValue(value: string): string {
  return value
    .replace(/\\/g, "\\\\")
    .replace(/\r\n?|\n/g, "\\n")
    .replace(/"/g, '\\"');
}

function labels(entries: Array<[string, string]>): string {
  return `{${entries.map(([name, value]) => `${name}="${escapeLabelValue(value)}"`).join(",")}}`;
}

/**
 * Render the current observability snapshot in the Prometheus text exposition format.
 *
 * Duration histograms are derived from each project's capped recent-trace window. They are
 * best-effort sliding-window distributions, not cumulative histograms across scrapes.
 */
export function renderPrometheus(summary: ObservabilitySummary): string {
  const lines: string[] = [];
  const projects = Object.values(summary.projects).sort((a, b) =>
    a.projectId.localeCompare(b.projectId),
  );

  lines.push(
    "# HELP ao_operations_total Total observed AO operations by project, metric, and outcome.",
    "# TYPE ao_operations_total counter",
  );
  for (const project of projects) {
    for (const [metric, counter] of Object.entries(project.metrics).sort(([a], [b]) =>
      a.localeCompare(b),
    )) {
      const commonLabels: Array<[string, string]> = [
        ["project", project.projectId],
        ["metric", metric],
      ];
      lines.push(
        `ao_operations_total${labels([...commonLabels, ["outcome", "success"]])} ${counter.success}`,
        `ao_operations_total${labels([...commonLabels, ["outcome", "failure"]])} ${counter.failure}`,
      );
    }
  }

  lines.push(
    "",
    "# HELP ao_overall_status Overall AO health status (0=ok, 1=warn, 2=error).",
    "# TYPE ao_overall_status gauge",
    `ao_overall_status ${STATUS_VALUE[summary.overallStatus]}`,
    "",
    "# HELP ao_health_status AO health surface status (0=ok, 1=warn, 2=error).",
    "# TYPE ao_health_status gauge",
  );
  for (const project of projects) {
    for (const health of Object.values(project.health).sort((a, b) =>
      a.surface.localeCompare(b.surface),
    )) {
      lines.push(
        `ao_health_status${labels([
          ["project", project.projectId],
          ["surface", health.surface],
        ])} ${STATUS_VALUE[health.status]}`,
      );
    }
  }

  lines.push(
    "",
    "# HELP ao_operation_duration_ms Best-effort AO operation duration distribution in milliseconds over the capped recent-trace window; values are not cumulative across scrapes.",
    "# TYPE ao_operation_duration_ms histogram",
  );
  for (const project of projects) {
    const durationsByOperation = new Map<string, number[]>();
    for (const trace of project.recentTraces) {
      if (
        typeof trace.durationMs !== "number" ||
        !Number.isFinite(trace.durationMs) ||
        trace.durationMs < 0
      ) {
        continue;
      }
      const durations = durationsByOperation.get(trace.operation) ?? [];
      durations.push(trace.durationMs);
      durationsByOperation.set(trace.operation, durations);
    }

    for (const [operation, durations] of [...durationsByOperation.entries()].sort(([a], [b]) =>
      a.localeCompare(b),
    )) {
      const commonLabels: Array<[string, string]> = [
        ["project", project.projectId],
        ["operation", operation],
      ];
      for (const upperBound of DURATION_BUCKETS_MS) {
        const count = durations.filter((duration) => duration <= upperBound).length;
        lines.push(
          `ao_operation_duration_ms_bucket${labels([
            ...commonLabels,
            ["le", String(upperBound)],
          ])} ${count}`,
        );
      }
      lines.push(
        `ao_operation_duration_ms_bucket${labels([...commonLabels, ["le", "+Inf"]])} ${durations.length}`,
        `ao_operation_duration_ms_sum${labels(commonLabels)} ${durations.reduce((sum, duration) => sum + duration, 0)}`,
        `ao_operation_duration_ms_count${labels(commonLabels)} ${durations.length}`,
      );
    }
  }

  return `${lines.join("\n")}\n`;
}
