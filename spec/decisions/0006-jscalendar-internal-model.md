# 0006 — JSCalendar (RFC 8984) as the internal model

- **Date**: 2026-11-10
- **Decided by**: author ("something truly cutting edge"), model choice by the spec author

The core speaks JSCalendar; CalDAV/iCalendar, REST and MCP are codecs of it. A JMAP adapter later becomes cheap.

Why: one model, many thin adapters; JSON-native for the REST API and agents.
