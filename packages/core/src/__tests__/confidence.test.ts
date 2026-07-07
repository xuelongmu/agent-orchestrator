import { describe, it, expect } from "vitest";
import { computeConfidence, summarizeConfidenceFactors } from "../confidence.js";

describe("computeConfidence", () => {
  it("returns full confidence with no risk signals", () => {
    const assessment = computeConfidence({});
    expect(assessment.score).toBe(1);
    expect(assessment.factors).toEqual([]);
  });

  it("ignores zero/absent signals", () => {
    const assessment = computeConfidence({
      reviewRounds: 0,
      ciFailureCount: 0,
      diffSize: 0,
      maxFindingSeverity: null,
      touchesSensitivePaths: false,
    });
    expect(assessment.score).toBe(1);
    expect(assessment.factors).toHaveLength(0);
  });

  it("penalizes review-round churn and caps the penalty", () => {
    const some = computeConfidence({ reviewRounds: 2 });
    expect(some.score).toBeCloseTo(0.76, 5);
    expect(some.factors[0].key).toBe("review_rounds");

    // Cap at 0.6 — many rounds cannot drive the penalty past the cap.
    const many = computeConfidence({ reviewRounds: 50 });
    expect(many.score).toBeCloseTo(0.4, 5);
  });

  it("weights open findings by severity and scales by finding confidence", () => {
    const error = computeConfidence({ maxFindingSeverity: "error" });
    expect(error.score).toBeCloseTo(0.5, 5);

    const warning = computeConfidence({ maxFindingSeverity: "warning" });
    expect(warning.score).toBeCloseTo(0.75, 5);

    // A low-confidence finding subtracts proportionally less.
    const scaled = computeConfidence({ maxFindingSeverity: "error", maxFindingConfidence: 0.4 });
    expect(scaled.score).toBeCloseTo(0.8, 5);
  });

  it("buckets diff size", () => {
    expect(computeConfidence({ diffSize: 50 }).score).toBe(1);
    expect(computeConfidence({ diffSize: 250 }).score).toBeCloseTo(0.9, 5);
    expect(computeConfidence({ diffSize: 700 }).score).toBeCloseTo(0.8, 5);
    expect(computeConfidence({ diffSize: 2000 }).score).toBeCloseTo(0.7, 5);
  });

  it("penalizes sensitive paths and CI failures", () => {
    expect(computeConfidence({ touchesSensitivePaths: true }).score).toBeCloseTo(0.7, 5);
    expect(computeConfidence({ ciFailureCount: 2 }).score).toBeCloseTo(0.8, 5);
    // CI penalty is capped at 0.3.
    expect(computeConfidence({ ciFailureCount: 20 }).score).toBeCloseTo(0.7, 5);
  });

  it("accumulates penalties across factors and clamps to zero", () => {
    const assessment = computeConfidence({
      reviewRounds: 3,
      maxFindingSeverity: "error",
      ciFailureCount: 3,
      diffSize: 1500,
      touchesSensitivePaths: true,
    });
    // 0.36 + 0.5 + 0.3 + 0.3 + 0.3 = 1.76 → clamped to 0.
    expect(assessment.score).toBe(0);
    // Factors are ordered by descending penalty.
    const penalties = assessment.factors.map((f) => f.penalty);
    expect(penalties).toEqual([...penalties].sort((a, b) => b - a));
  });

  it("summarizes factors as a human-readable string", () => {
    const assessment = computeConfidence({ reviewRounds: 2, maxFindingSeverity: "error" });
    const summary = summarizeConfidenceFactors(assessment);
    expect(summary).toContain("review→fix round");
    expect(summary).toContain("error review finding");

    expect(summarizeConfidenceFactors(computeConfidence({}))).toBe("no risk factors detected");
  });
});
