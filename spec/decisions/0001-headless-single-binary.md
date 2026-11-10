# 0001 — Headless, single Go binary

- **Date**: 2026-11-10
- **Decided by**: author

One Go binary (`pcb`) runs the daemon and the CLI. Everything, install to uninstall, works from a terminal, on a host without a display. GUIs are later clients of the local API.

Why: the bridge must run on servers (morpheus) as well as desktops, and be drivable by agents.
