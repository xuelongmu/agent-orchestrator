---
"@aoagents/ao-core": minor
"@aoagents/ao-plugin-tracker-linear": minor
"@aoagents/ao-plugin-tracker-github": minor
"@aoagents/ao-plugin-tracker-gitlab": minor
---

feat(core,tracker): model parent/child + blocking relations

Extend the tracker contract with issue hierarchy and dependency relations:

- `CreateIssueInput` gains `parentId`, `blockedBy`, and `relatedTo`.
- `Issue` gains `parentId`, `children`, `blockedBy`, `blocks`, and `relatedTo`.
- Linear sets the parent on create, creates `blocks`/`related` relations via
  `issueRelationCreate`, and returns hierarchy + relations from `getIssue`.
- GitHub and GitLab emulate relations (best-effort) via a body convention
  (`Part of #N` / `Blocked by #N` / `Related to #N`) that round-trips through
  `getIssue`.
