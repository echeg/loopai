# T3 bridge title-regeneration wait outlasts the before-exit stop

- found: 2026-10-25, plan: 20261025-t3-thread-done-badge, phase: internal review
- severity: minor
- area: cmd/loopai/t3_bridge.go

`syncTitle` (cmd/loopai/t3_bridge.go:527) holds the first title write for up to `regenWait`
(2 minutes, `defaultT3BridgeTiming`, line 89) while T3 Code is still generating the thread's title.
The detached child stops the bridge through `beforeExit` with `t3BridgeStopTimeout` (10 s, line 22),
and `stop` cancels the bridge without `closeOut`. A run that finishes before its first title write
while a regeneration is pending, such as one that fails right after the start, therefore exits with
neither the final `[loopai: <state>]` title nor, since `syncUnread` waits for that title, the
mark-unread that gives the thread its "Done" badge.

The title half predates the mark-unread; the mark-unread inherits it by design, because a mark
written before the final title would be cleared when that title gets an open thread visited again.

Fix direction: once the attempt has finished, stop waiting for the regeneration (write the final
title at once, accepting that it cancels T3's generation), or let `stop` run a bounded `closeOut`
with a fresh context when its timeout fires before the bridge ended by itself.
