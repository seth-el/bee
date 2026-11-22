# Platform support

Bee is supported on **Linux**. The supported runtime and acceptance gates target Linux hosts with the Unix process, filesystem, locking, user-namespace, and Bubblewrap behavior used by the foreground runtime. `bwrap` is required for ordinary shell write-root enforcement and Bee fails closed when it is unavailable. NixOS operators should distinguish a missing command (`pkgs.bubblewrap` in `environment.systemPackages`) from an existing dynamically linked executable whose loader is unavailable (`programs.nix-ld`). The explicit `assistant.unsafe_disable_process_sandbox` option is documented in [permissions.md](permissions.md); it is a security downgrade, not an automatic portability fallback.

**Windows is explicitly out of scope for this project.** Windows builds, Windows process-group cleanup, and Windows-specific filesystem/locking behavior are not replacement-readiness targets. The Windows process-group implementation remains a deliberate deferred stub and must not be interpreted as supported behavior.

This scope is intentional, not an invitation to silently weaken safeguards for portability. A future Windows port would require its own design and review for:

- cross-process session and scheduler locking;
- child process-tree cancellation and cleanup;
- private-root and permission semantics;
- clean-root backup/restore and recovery acceptance;
- equivalent coding-mode and approval invariants.

Do not claim Bee replacement readiness on Windows or add Windows compatibility work as incidental cleanup. Linux is the project platform until a separately approved platform scope changes this contract.
