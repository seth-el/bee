# Bee operations delegation (phase 4A)

Bee exposes a parent-owned `delegate_operations` seam for validated,
explicitly bounded `OpsPlan`/`OpsTask` work. Targets are exact tagged values:

- Kubernetes: `context`, `namespace`, `resource`, `name` (optional container)
- SSH/VM: exact `host`, `user`, `port` (optional inventory `vm_id`)
- local/container: explicit runtime and container name

## Capability projection

An ACP operations adapter receives an `OpsCapabilityProjection`, never Bee's
general `RemoteCapabilityGrant`, memory projection, secrets, filesystem scope,
or full environment. The projection contains only one exact target, an
allowlist of narrow capabilities (`read`, `observe`, `execute`, optional
`stdin`, `mutate`, and `rollback`), and parent-owned ceilings for time,
commands, steps, and output.

Projection validation is fail-closed:

- requested and projected targets must match exactly;
- unknown capabilities are rejected;
- read-only scouts cannot project or execute mutation;
- mutating tasks require both `mutate` and `rollback` capabilities;
- task budgets cannot exceed projection ceilings;
- stdin requires an explicit `stdin` capability.

## ACP operations backend

`NewOpsACPBackend` and `NewOpsACPReadOnlyScoutBackend` construct the explicit
provider-neutral ACP adapter. `ExecutePlan` consumes a validated plan plus projection and
renders a bounded `bee.ops.v1` JSON packet. The packet carries only goal,
objective, exact target, command argv, criteria, and projected capabilities.
The backend uses a fresh private temporary working directory and clears the
ACP environment allowlist; no real command runner is embedded here. ACP
terminal/filesystem capabilities are not advertised, and permission requests
remain denied by the transport's default fail-closed behavior.

`OpsCompletion` is compact descriptive evidence. ACP text, info, artifacts,
and diffs are bounded; mismatched task/parent identifiers fail closed. The
parent remains responsible for approval, execution authority, rollback, and
acceptance.

## Product-wired read-only diagnostics

The primary Bee config can explicitly enable the parent-owned command backend.
It binds one exact target projection and keeps mutations disabled:

```json
{
  "operations": {
    "enabled": true,
    "backend": "local-child",
    "target": {
      "kind": "kubernetes",
      "context": "lab",
      "namespace": "cert-manager",
      "resource": "certificate",
      "name": "api-cert"
    },
    "capabilities": ["read", "execute"],
    "read_only_scout": true,
    "max_timeout_seconds": 30,
    "max_output_bytes": 65536
  }
}
```

A cert-manager diagnostic request is represented as a read-only task such as
`["get", "certificate/api-cert", "-o", "wide"]`. Bee's parent converts this
to `kubectl --context lab --namespace cert-manager get certificate/api-cert -o
wide` and runs it through the existing bounded argv command authority. The
local-child route receives only the resulting bounded `OpsCompletion` evidence;
it does not receive kubeconfig, credentials, environment variables, memory, or
command authority. Kubernetes context/namespace and resource/name overrides are
rejected, as are mutating Kubernetes verbs and shell escape arguments.

The backend also supports exact SSH/VM and local-container target bindings for
read-only diagnostics. No real SSH, Kubernetes, VM, or container mutation is
authorized or exercised by the tests; fake command runners cover Kubernetes
argv binding, target mismatch, mutation denial, secret-environment stripping,
budget bounds, and normal assistant tool exposure.

## Phase 4A supervised SSH/VM mutation broker

Phase 4A adds a deliberately separate parent-owned mutation path through
`NewOpsSSHMutationCommandBackend`. It is disabled unless the composition root
supplies all of the following:

- an exact SSH/VM target (`host`, `user`, `port`, optional inventory ID);
- a parent-owned `OpsMutationGrant` with a grant ID, auditable approval ID and
  reason, explicit `mutate`/`execute`/`rollback` capabilities, allowlisted
  operation classes, expiry or one-shot semantics, bounded timeout/argv/step/
  output ceilings, and optional explicit stdin permission;
- `OperationsDelegationService.AllowMutations = true`; and
- a backend implementing the mutation-authorizer seam.

The grant is never placed on `OpsTask` or serialized into an ACP packet. Child
ACP/local-child receives only the plan/task, operation class, and bounded
descriptive completion evidence. The parent strips inherited environment
allowlists, remote grants, and trusted-foreground authority before invoking the
dedicated parent-owned SSH argv runner, which retains exact host/user/port
binding and strict host-key checking. Shell interpreters, shell strings,
arbitrary executables, target overrides, unsafe argv, unapproved stdin, expired
grants, and reused one-shot grants are rejected before the fake/real runner is
reached. `PreviewOpsMutation` provides a dry-run representation for a future
human approval surface without executing anything.

### Disposable game-server VM example

For a disposable VM already provisioned in the parent inventory, a human can
approve one bounded service action and then compose it explicitly:

```go
target := bee.OpsTarget{Kind: bee.OpsTargetSSHVM, SSHVM: &bee.SSHVMTarget{
    Host: "192.0.2.44", User: "ops", Port: 22, VMID: "game-disposable-01",
}}
grant := &bee.OpsMutationGrant{
    GrantID: "grant-game-01", ApprovalID: "approval-game-01",
    ApprovalReason: "restart the disposable game service after deployment",
    Target: target,
    Capabilities: []bee.OpsCapability{bee.OpsCapabilityExecute, bee.OpsCapabilityMutate, bee.OpsCapabilityRollback},
    OperationClasses: []string{bee.OpsOperationClassSSHVMService}, OneShot: true,
    MaxTimeoutSeconds: 30, MaxCommands: 8, MaxSteps: 1, MaxOutputBytes: 65536,
}
backend, _ := bee.NewOpsSSHMutationCommandBackend(scope, projection, grant)
```

The only accepted command for this narrow example is an argv-shaped action
such as `[]string{"systemctl", "restart", "game-server"}`. Bee does not run a
deployment shell, copy credentials, or infer a VM inventory record from the
child. Kubernetes mutation execution is the next slice; this phase supplies
only the shared approval/preview/completion contract for it.
