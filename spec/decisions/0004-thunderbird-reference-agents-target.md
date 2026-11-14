# 0004 — Thunderbird is the reference client; AI agents are the main consumer

- **Date**: 2026-11-10
- **Decided by**: author

CalDAV is validated against Thunderbird first. The long-term main use is as a source for AI agents, through an MCP adapter on the core (not a CalDAV-to-MCP layer).

Why: Thunderbird is the author's checking client; agents need the full model, which CalDAV would flatten.
