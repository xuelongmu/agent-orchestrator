/**
 * Confidence scoring for autonomous actions (#12).
 *
 * AO's escalation was historically attempt/duration-based: a reaction escalated
 * only after N failed retries or a time budget. There was no notion of AO
 * deciding "I'm not confident enough — ask the human" before acting.
 *
 * `computeConfidence` folds a handful of cheap, already-available risk signals
 * into a single 0..1 score. The lifecycle manager gates opt-in auto-actions
 * (auto-merge, auto-fix, spawn-dependent) on it: below a configured
 * `confidenceThreshold`, the action is held and escalated to a human with a
 * question instead of running.
 *
 * The heuristic is deliberately simple and transparent — it starts at full
 * confidence (1.0) and subtracts a penalty per risk factor, keeping every
 * factor visible so the escalation can explain *why* it held.
 */

import type { CodeReviewSeverity } from "./code-review-store.js";

/** Risk signals gathered for a session before an autonomous action. */
export interface ConfidenceSignals {
  /** Number of review→fix rounds already spent on this PR (churn proxy). */
  reviewRounds?: number;
  /** Highest severity among open automated (Codex) review findings. */
  maxFindingSeverity?: CodeReviewSeverity | null;
  /** Confidence (0..1) of the highest-severity open finding, if reported. */
  maxFindingConfidence?: number | null;
  /** How many times CI has failed / been retried for this session. */
  ciFailureCount?: number;
  /** Total lines changed in the PR diff (additions + deletions). */
  diffSize?: number;
  /** Whether the change touches sensitivity-flagged paths. */
  touchesSensitivePaths?: boolean;
}

/** A single reason the confidence score was reduced. */
export interface ConfidenceFactor {
  /** Machine key, e.g. "review_rounds". */
  key: string;
  /** Human-readable reason the confidence was reduced. */
  detail: string;
  /** How much this factor reduced the score (0..1). */
  penalty: number;
}

/** Result of scoring: a clamped score plus the factors that shaped it. */
export interface ConfidenceAssessment {
  /** Final confidence score, clamped to [0, 1]. */
  score: number;
  /** Factors that reduced confidence, highest penalty first. */
  factors: ConfidenceFactor[];
}

/** Penalty weights — small, hand-tuned, and intentionally conservative. */
const PER_REVIEW_ROUND_PENALTY = 0.12;
const MAX_REVIEW_ROUND_PENALTY = 0.6;
const SEVERITY_WEIGHT: Record<CodeReviewSeverity, number> = {
  error: 0.5,
  warning: 0.25,
  info: 0.05,
};
const PER_CI_FAILURE_PENALTY = 0.1;
const MAX_CI_FAILURE_PENALTY = 0.3;
const SENSITIVE_PATH_PENALTY = 0.3;
// Diff-size buckets (lines changed → penalty). Larger diffs are riskier to
// auto-act on; the buckets keep the heuristic legible.
const DIFF_SIZE_BUCKETS: ReadonlyArray<{ min: number; penalty: number; label: string }> = [
  { min: 1000, penalty: 0.3, label: "very large" },
  { min: 500, penalty: 0.2, label: "large" },
  { min: 200, penalty: 0.1, label: "sizeable" },
];

function clamp01(value: number): number {
  if (value < 0) return 0;
  if (value > 1) return 1;
  return value;
}

/**
 * Fold risk signals into a 0..1 confidence score.
 *
 * Pure and deterministic: score starts at 1.0 and each supplied signal can only
 * subtract confidence. Absent signals contribute nothing.
 */
export function computeConfidence(signals: ConfidenceSignals): ConfidenceAssessment {
  const factors: ConfidenceFactor[] = [];

  const reviewRounds = signals.reviewRounds ?? 0;
  if (reviewRounds > 0) {
    const penalty = Math.min(reviewRounds * PER_REVIEW_ROUND_PENALTY, MAX_REVIEW_ROUND_PENALTY);
    factors.push({
      key: "review_rounds",
      detail: `${reviewRounds} review→fix round${reviewRounds === 1 ? "" : "s"} so far`,
      penalty,
    });
  }

  if (signals.maxFindingSeverity) {
    const findingConfidence = clamp01(signals.maxFindingConfidence ?? 1);
    const penalty = SEVERITY_WEIGHT[signals.maxFindingSeverity] * findingConfidence;
    if (penalty > 0) {
      const confidenceSuffix =
        signals.maxFindingConfidence != null
          ? ` (confidence ${Math.round(findingConfidence * 100)}%)`
          : "";
      factors.push({
        key: "open_findings",
        detail: `open ${signals.maxFindingSeverity} review finding${confidenceSuffix}`,
        penalty,
      });
    }
  }

  const ciFailureCount = signals.ciFailureCount ?? 0;
  if (ciFailureCount > 0) {
    const penalty = Math.min(ciFailureCount * PER_CI_FAILURE_PENALTY, MAX_CI_FAILURE_PENALTY);
    factors.push({
      key: "ci_failures",
      detail: `${ciFailureCount} CI failure${ciFailureCount === 1 ? "" : "s"} on this branch`,
      penalty,
    });
  }

  if (typeof signals.diffSize === "number" && signals.diffSize > 0) {
    const bucket = DIFF_SIZE_BUCKETS.find((b) => signals.diffSize! >= b.min);
    if (bucket) {
      factors.push({
        key: "diff_size",
        detail: `${bucket.label} diff (${signals.diffSize} lines changed)`,
        penalty: bucket.penalty,
      });
    }
  }

  if (signals.touchesSensitivePaths) {
    factors.push({
      key: "sensitive_paths",
      detail: "touches sensitivity-flagged paths",
      penalty: SENSITIVE_PATH_PENALTY,
    });
  }

  factors.sort((a, b) => b.penalty - a.penalty);
  const totalPenalty = factors.reduce((sum, f) => sum + f.penalty, 0);
  return { score: clamp01(1 - totalPenalty), factors };
}

/** One-line, human-readable summary of the factors that lowered confidence. */
export function summarizeConfidenceFactors(assessment: ConfidenceAssessment): string {
  if (assessment.factors.length === 0) return "no risk factors detected";
  return assessment.factors.map((f) => f.detail).join("; ");
}
