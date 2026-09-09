# acp-kit backlog

Known problems and deferred work. Each entry states the defect, the
evidence, and what a fix would have to do — not a vague wish.

## `skills.LoadBuiltinIn` prunes other processes' extractions

`LoadBuiltinIn` sweeps its parent directory (and the legacy `$TMPDIR`
location) with `keep=""` for the current generation, removing every
`<relay>-*` extraction it did not just create. Its doc justifies this by
asserting that this process is the only live owner of those directories.

That assertion is false on a multi-relay host. Several `poe-acp` workers
— different bots, different versions, staged updates — run co-located and
share `$TMPDIR`. A newly started worker's sweep deletes the extraction a
co-located, not-yet-updated worker is still reading skills out of.

A fix has to stop treating "not mine" as "dead". Options: keep any
generation whose directory is still open/mtime-fresh, take a per-directory
lock for the lifetime of the owning process, or drop the legacy `$TMPDIR`
sweep entirely and prune only under the caller's own state dir (where
ownership really is exclusive).

Not urgent: the state-dir move in v0.14.0 means new versions no longer
land in `$TMPDIR` at all, so the blast radius shrinks with every host
that updates.
