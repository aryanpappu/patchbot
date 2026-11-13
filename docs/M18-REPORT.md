# M18 report: deploy rehearsal on Oracle Mumbai (2026-11-13)

Target: Oracle Mumbai CyberPanel host (arm64, Ubuntu 24.04), shared with stock.mishra.eu.org,
rkado-bots and CyberPanel. Instance will shrink to 2 OCPU / 12 GB by ~2026-12-10; caps sized for that.

## Isolation from CyberPanel
- System user `patchbot` (uid 995, nologin), home `/srv/patchbot` mode 0700, in no panel/site group.
- Rootless podman 4.9.3 (subuid 200000-265535, linger on). Not in any docker group; no rootful socket.
- `user-995.slice`: CPUQuota=80%, MemoryMax=3G, TasksMax=1024 (all containers combined).
- Service: CPUQuota=50%, MemoryMax=768M, ProtectSystem=strict, ProtectHome=read-only
  (`yes` hides /run/user and breaks rootless podman), NoNewPrivileges, PrivateTmp, UMask=0077.
- Host git upgraded 2.43.0 -> 2.56.1 (ppa:git-core).

## Access
- Binds 127.0.0.1:8080; reached only via Cloudflare Tunnel `mumbai-panel` ingress
  `patchbot.mishra.eu.org` (Host rewritten to 127.0.0.1:8080), behind Access app "patchbot"
  (policy Owner only). No new public port.
- Checks: public URL -> 302 to Access login; `/api/runs` without bearer -> 401; foreign Host -> 403;
  CyberPanel hostname still 302 to its own Access app.

## Checks
- `patchbot doctor` under the service sandbox: all PASS except missing credentials (owner to set).
- M17 integration suite (9 tests) PASS on this host as user patchbot; zero stray containers.
- Limits verified in-container under the unit's sandbox: memory.max and pids.max applied.
- Backup: nightly cron 03:17, `sqlite3 .backup` + integrity_check, 14-day retention, 0600 files.
  Restore drill into a fresh data dir: migrations 4/4 checksums match, `runs` opens. PASS.

## Pending (M19, needs owner credentials)
Live run, restart during a run (stale recovery), queue saturation, site latency under load,
obsolete approval, repair, cancel, PR ambiguity.
