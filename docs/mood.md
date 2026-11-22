# Mood continuity

Bee's optional `memory/mood.md` is one application-wide, prose-only hint about
affective register. It is not durable-memory index data, session history, a
numeric emotion model, or authority over the live conversation.

## Existing composition boundary

Parent agents compose the built-in capability prompt, optional `SOUL.md`, and
delimited `BEE.md`. Durable turns currently construct a fresh `Agent`, while a
store-less application reuses one process agent. Structured memory does not
enter that stable prefix: it is available through `memory_search` and through
explicitly configured transient recall.

Mood is loaded from the configured memory root, normally
`~/.bee/workspace/memory/mood.md`, at a session-activation seam. Repeated turns
in the same selected session reuse the exact in-memory snapshot. Switching
sessions, or explicitly selecting/resuming the same durable session, creates a
new activation and may read a newer baseline. Child-role agents do not receive
the mood file.

The file is bounded, owner-only, regular, and non-symlink. Missing, empty,
unsafe, unreadable, oversized, or malformed content composes as absent and can
never prevent a session from starting. Bee never logs its contents.

## Appending movement and consolidating at seams

The model uses the existing policy/plan/audit-controlled filesystem tools; Bee
does not expose a mood-specific write capability. A complete delta block may
be appended to a dated note such as `memory/2026-10-29.md`, or to the dedicated
append-only `memory/mood-deltas.md` note:

```markdown
<!-- bee:mood-delta at="2026-10-29T14:30:00Z" decay="short" -->
Still a little stung by the abrupt exchange, but the repair helped.
<!-- /bee:mood-delta -->
```

Allowed natural decay classes are `fleeting`, `short`, `medium`, and `long`.
Delta prose is bounded to 1024 bytes. Timestamps are RFC3339. Incomplete,
future-dated, malformed, oversized, or reserved-marker-bearing blocks are not
merged.

The qualitative classes have documented operational lifetimes: `fleeting` is
six hours, `short` is two days, `medium` is fourteen days, and `long` is sixty
days. Before one lifetime, prose is carried unchanged. From one through two
lifetimes, it is rendered as "Fading, but still present" rather than treated as
equally current. At two lifetimes it is dropped from the active baseline. This
metadata is about retention, never an emotion score. Operator-authored prose
outside Bee's managed section is a stable baseline and is not guessed at or
decayed automatically.

On the first turn of a new session activation, Bee scans bounded daily/delta
notes and merges complete blocks oldest-first before taking that session's
snapshot. Source notes are never rewritten. `mood.md` embeds content-free
append cursors and entry metadata in a managed block; a cross-process lock and
one atomic replacement commit the baseline and cursors together. A crash
before replacement leaves every source delta eligible for retry, while a
completed replacement makes a repeated sweep at the same time a no-op. Dropped
entries remain covered by their append cursors and therefore cannot reappear
on the next sweep.

For sessions that ended without another activation seam, run the same
idempotent operation from cron or a systemd user timer:

```sh
bee memory mood-sweep --memory-root "$HOME/.bee/workspace/memory"
```

The JSON report contains counts and update state only. It never prints mood
prose or source-note contents.

## What a session sees, and how much stays long-lived

`mood.md` keeps every carried entry, but a session's prefix shows only the
newest ones: at most `MaxMoodSnapshotEntries` (10) entries and
`MaxMoodSnapshotBytes` (4096) of entry text, oldest first, always including
the newest. When entries are left out, the snapshot ends with a one-line
count of them. Mood is a hint about current register; older movements stay
in the file and in the daily notes rather than in every prompt.

Only the newest `MaxMoodLongEntries` (8) entries tagged `long` keep the
60-day half-life. An older `long` entry decays as `medium` (softened at 14
days, dropped from the baseline at 28). The delta in its source note is
never touched. Without this limit a baseline written mostly as `long` would never
fade.

## Manual verification

1. Create an owner-only `memory/mood.md` with a short prose baseline, start a
   new parent session, and confirm the provider request contains one bounded
   `<bee-mood-context>` after the ordinary SOUL/BEE prefix.
2. Append a valid delta block to today's dated memory note with the existing
   filesystem tools. Continue the same selected session and confirm the
   checksum of `mood.md` and the active prompt snapshot do not change.
3. Start or explicitly resume another session. Confirm `mood.md` is replaced
   atomically, the delta appears once in the new session's prefix, and the
   dated source note remains byte-for-byte unchanged.
4. Run `bee memory mood-sweep` twice. The first content-free JSON report should
   show the merge/update; the second should report no merge and no update.
5. Exercise one entry in each decay class using controlled timestamps. Confirm
   a one-lifetime entry reads "Fading, but still present" and a two-lifetime
   entry is absent without reappearing on the next sweep.
6. Temporarily remove `mood.md`, then try an unsafe-permission and malformed
   managed file. In every case a parent turn must still start with mood omitted,
   and no mood prose should appear in logs or maintenance output.
