import { NextResponse, type NextRequest } from "next/server";
import { renderPrometheus } from "@/lib/metrics-prometheus";
import { getServices } from "@/lib/services";
import {
  getCorrelationId,
  getObservabilitySummary,
  recordApiObservation,
} from "@/lib/observability";

const CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8";

export async function GET(request: NextRequest) {
  const correlationId = getCorrelationId(request);
  const startedAt = Date.now();

  try {
    const { config } = await getServices();
    const summary = getObservabilitySummary(config);
    const body = renderPrometheus(summary);
    recordApiObservation({
      config,
      method: "GET",
      path: "/api/metrics",
      correlationId,
      startedAt,
      outcome: "success",
      statusCode: 200,
      data: {
        projectCount: Object.keys(summary.projects).length,
        overallStatus: summary.overallStatus,
      },
    });
    return new NextResponse(body, {
      status: 200,
      headers: {
        "Content-Type": CONTENT_TYPE,
        "x-correlation-id": correlationId,
      },
    });
  } catch (err) {
    const { config } = await getServices().catch(() => ({ config: undefined }));
    if (config) {
      recordApiObservation({
        config,
        method: "GET",
        path: "/api/metrics",
        correlationId,
        startedAt,
        outcome: "failure",
        statusCode: 500,
        reason: err instanceof Error ? err.message : "Failed to render metrics",
      });
    }
    return new NextResponse("# Failed to render AO metrics\n", {
      status: 500,
      headers: {
        "Content-Type": CONTENT_TYPE,
        "x-correlation-id": correlationId,
      },
    });
  }
}
