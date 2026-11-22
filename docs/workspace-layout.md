# Bee workspace layout

Bee uses a personal workspace model separate from the source checkout.
`BEE_HOME` may override the root for tests, portable profiles, or another
machine; the normal Linux default is `~/.bee`.

```text
~/.bee/
├── config.json                 # 0600, provider/profile configuration
├── secrets/                    # 0700; individual secret files are 0600
│   └── openrouter-api-key
├── state/                      # 0700; runtime state outside the Git workspace
│   ├── sessions/
│   └── runtime/
└── workspace/                  # 0700; private Git-backed personal context
    ├── SOUL.md                 # optional 0600 personal system-prompt file
    ├── BEE.md                  # parent-session workspace context
    ├── memory/                 # durable Bee memory/events
    └── projects/                 # canonical root for opt-in project directories
```

The source repository remains separate, for example `~/projects/bee` or the
current development checkout. The workspace is user data, not source code.

`BEE.md` is the single human-facing context file for identity, human profile,
runtime boundaries, and workspace instructions. At parent-agent construction,
Bee reads the canonical file, validates that it is a regular non-symlink,
owner-only readable file no larger than 64 KiB, and appends it after the
effective system prompt as delimited operator-authored context. It is re-read
when a durable turn creates an agent, is not copied into session history, and
is not projected to child-role agents. An explicitly empty
`agent.system_prompt` retains its legacy
meaning and disables the complete system message, including BEE.md.

Bee may still preserve identity and profile data as separate typed domains
internally; this human-facing file does not merge them into durable memory or
the session event log. Keep secrets out of BEE.md.

Live memory belongs in `workspace/memory`, including model-maintained dated
memory files when those are useful. Sessions, locks, caches, and other transient
runtime state belong under `state`. Secrets never belong in the Git-backed
workspace. Prefer explicit protected secret files over automatically sourcing
arbitrary `.env` files.

Migration is performed with Bee export/import or backup/restore. Moving the
source checkout does not move or invalidate the workspace. A private Git repo
may back up `workspace`; it should not contain secrets or unreviewed runtime
logs.

Operator-configured paths for system prompts, speech helpers, semantic indexes,
and semantic key files may be relative to `BEE_HOME`. Prefer those relative
forms in a profile intended to move between machines. Session/project metadata
can still contain historical absolute project roots; semantic indexes are
derived state and should be rebuilt after migration.
