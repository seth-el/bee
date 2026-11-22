# Backup and restore

Create a bounded backup to a new destination:

```sh
bee maintenance backup --ops-config /private/bee/ops.json --destination /secure/backups/bee.tar.gz
```

Restore is dry-run by default:

```sh
bee maintenance restore --ops-config /private/bee/ops.json --source /secure/backups/bee.tar.gz
```

Review conflicts. Apply only with explicit approval:

```sh
bee maintenance restore --ops-config /private/bee/ops.json --source /secure/backups/bee.tar.gz --apply
```

Secrets are not in normal backups and must be provisioned separately. Never overwrite a differing destination file without resolving the conflict and preserving the prior backup.

Session summary/search sidecars (`*.jsonl.idx.json`) and artifact/attachment
`.session-index/` files are rebuildable caches; JSONL and artifact/attachment
metadata remain authoritative. Backups may contain these sidecars. A restored
stale sidecar is safe because normal reads validate source metadata and rebuild
it, but operators may remove only these cache files while Bee is stopped if a
forced rebuild is needed. Never delete the authoritative JSONL, artifact
directories, attachment metadata, or active `.lock` files as an index repair.
