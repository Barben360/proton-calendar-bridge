# Open questions for the author

From the builder review of SPEC.md draft v1 (2026-11-10), most important first.

1. **Client identity (E03).** If Proton only grants calendar scope to an
   `x-pm-appversion` copied from an official Proton client, do we accept
   sending it? Decides whether the project is viable as designed (M0 gate).
2. **Where it runs and who reaches it.** Desktop next to Thunderbird,
   morpheus reached over LAN/tailnet, or both from v1? And do agents run as
   your OS user, another user, or another host? Decides when remote access
   (TLS, network auth, bind address) lands (M3 vs M5) and the default
   install (user vs system service).
3. **Roadmap order.** Keep MCP last (M7), or a read-only MCP with grants
   right after sync (M2), before CalDAV writes?
4. **Account size.** Roughly how many events, and how far back, in your
   real calendar; and is two-password mode on? Decides whether search can
   stay memory-only in v1 (E09) and which login path matters most.
