---
"@aoagents/ao-plugin-scm-github": patch
---

Recognize `chatgpt-codex-connector[bot]` as an automated review bot so Codex PR review comments route through the bot (`bugbot-comments`) dispatch path, consistent with other AI reviewers like `cursor[bot]`.
