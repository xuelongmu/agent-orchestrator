-- Keep the stable local telemetry intent addressable from its review run so
-- retention can preserve it until worker delivery is durably complete.
--
-- Upgrade note: before this migration, simplification_dispatched_at was only
-- set by MarkReviewRunDelivered in the same UPDATE that set delivered_at and
-- status='delivered'. A delivery-stamp failure rolled back all three fields, so
-- released schemas cannot contain an undelivered row with a preexisting
-- simplification receipt. Existing non-NULL receipts are therefore delivered
-- and intentionally keep the empty event id so normal retention applies.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN simplification_event_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_review_run_pending_simplification_event
    ON review_run(simplification_event_id)
    WHERE delivered_at IS NULL AND simplification_event_id != '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_review_run_pending_simplification_event;
ALTER TABLE review_run DROP COLUMN simplification_event_id;
-- +goose StatementEnd
