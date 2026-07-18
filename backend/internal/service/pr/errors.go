package pr

import "errors"

// Sentinel errors returned by the PR action service.
var (
	ErrInvalidPR            = errors.New("pr: invalid request")
	ErrPRNotFound           = errors.New("pr: not found")
	ErrPRNotMergeable       = errors.New("pr: not mergeable")
	ErrPRHeadChanged        = errors.New("pr: head changed")
	ErrPRPreconditions      = errors.New("pr: merge preconditions unmet")
	ErrNothingToResolve     = errors.New("pr: nothing to resolve")
	ErrReviewThreadNotFound = errors.New("pr: review thread not found")
	ErrPRPermissionDenied   = errors.New("pr: permission denied")
	ErrActionNotConfigured  = errors.New("pr: action not configured")
)
