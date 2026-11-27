# Shared state and upgrades

## Two active managers

The Helm chart defaults to `state.mode: redis` and `replicaCount: 2`. Both replicas accept requests without session affinity. Required hostname anti-affinity places them on different eligible nodes. Rolling updates replace one replica at a time, with `maxSurge: 0` so a rollout fits on two hosts. The PodDisruptionBudget allows one voluntary eviction at a time, while the Deployment strategy controls the rollout. With a node unavailable, the remaining replica can serve requests while Kubernetes waits for another eligible node.

Point every replica at the same writable Redis 7+ or Valkey endpoint. Put the connection URL in the key named by `state.redisSecretKey` in `existingSecret`. The default key is `CDN_MANAGER_REDIS_URL`. Use `rediss://` for TLS with the endpoint's trusted CA, or restrict network access to a private endpoint. The state key prefix derives from the Helm namespace and release name. Set `state.redisPrefix` explicitly if relocating or operating multiple installations against one Redis database. Keep the prefix the same on every replica and dedicate it to this installation.

Run the endpoint with durable persistence, normally AOF with `appendfsync always`, `maxmemory-policy noeviction`, and persistent storage. Prevent clients outside the installation from modifying its keys. An endpoint that acknowledges writes before persisting them or promotes a replica with stale data can lose a completion intent or revive revoked authentication state. Use backups for disaster recovery. After restoring from an older backup, stop the managers and expire their `<prefix>:auth:*` keys before reopening access. That includes old logins, sessions, and transfer grants.

A single Valkey instance is a shared dependency. It can restart on its persistent volume, but loss of that instance makes the managers unavailable until the store recovers. Readiness returns 503 during that period. Web authorization, session changes, MCP transfer grants, and multipart operations fail closed. Existing JWT-only MCP calls that require neither an opaque grant nor multipart state can still be authorized when sent directly to a replica. The public CDN gateway is outside this dependency.

### Optional Valkey on this chart

Set `valkey.enabled: true` and `valkey.existingSecret` to the name of a Secret containing a `users.acl` file. Use a unique password. For example, enable an application account in the ACL and disable the default account.

```text
user default reset off
user cdn on >replace-with-a-unique-password ~cdn-manager:* +ping +hello +select +eval +evalsha +time +exists +get +set +del +hget +hkeys +hset +hdel +pexpire +pexpireat +zadd +zcard +zcount +zrangebyscore +zrem +zremrangebyscore +zscore
```

This example assumes the dedicated Valkey instance and `state.redisPrefix: cdn-manager`. Use an access policy appropriate to your environment and match the key scope to your configured prefix. Set the manager's Redis URL to `redis://cdn:<encoded-password>@<release-fullname>-valkey:6379/0`. Verify the Service name with `helm template` if the release name is truncated. The chart provisions a single StatefulSet with an AOF volume and limits ingress to pods bearing the manager's selector in the same namespace. ACL authentication still applies. This option leaves the datastore as a single point of failure.

The starting capacity is 32 MiB `maxmemory`, 32 MiB Kubernetes memory request, 128 MiB memory limit, and a 1 GiB PVC. In a local Ubuntu test, Valkey 9.2.0 used 1.12 MiB of allocator memory and 10.63 MiB RSS when idle. At 96 live OIDC sessions and 96 active multipart sessions with one part recorded each, it used 1.84 MiB and 11.13 MiB RSS. These are fixture measurements, not capacity limits. Size production for your concurrent sessions, multipart part counts, AOF rewrite peak, and PVC usage.

### Multipart recovery

Redis stores upload identity and ownership, immutable part SHA-256 fingerprints, the completion intent, and the final response. S3 stores all payload bytes. A delayed writer may retry the same part bytes but cannot replace them with different bytes after its lease expires. Completion fixes the part manifest before the S3 call. Abort and completion cannot displace one another. After a lost completion reply, the manager reconciles the provider upload and checks the object marker before returning the stored result. This relies on the provider preserving the object marker until recovery. Avoid replacing or deleting that same key while a completion outcome is unknown, including through direct S3 access.

Enable provider-side cleanup of abandoned incomplete multipart uploads. The manager sweeps expired entries with known provider upload IDs. A process can be lost after S3 assigns an ID and before the manager publishes it to Redis. A provider rule such as S3 `AbortIncompleteMultipartUpload` or a maintenance job can clean up those uploads without requiring the missing ID. For Garage 2.3.0, the administrative command is `garage bucket cleanup-incomplete-uploads --older-than 2d <bucket>`. Apply it to a bucket managed by the installation only after verifying the threshold against other writers' upload durations. The manager's active upload lifetime is 24 hours.

## Upgrade from the 0.1.0 file journal

To retain existing upload sessions, import the journal while both versions are stopped. Users sign in again because 0.1.0 kept web authorization in memory.

1. Back up the legacy PVC and retain it through the upgrade. If a GitOps controller owns the claim, arrange retention before changing its desired state. Stop the old manager and wait for its pod to terminate and release the claim. Keep the new Deployment stopped until import succeeds.
2. Launch a one-shot Job or maintenance container with the new release image. Mount the old PVC read/write at `/legacy-state` and run the container as the same UID/GID that owns the journal. Mount or inject the same Redis endpoint and prefix the new managers will use. Read `CDN_MANAGER_REDIS_PREFIX` from the rendered Deployment or set a specific `state.redisPrefix` in your values.
3. Pass `import-file-state /legacy-state` as the image arguments. The importer locks the old directory and copies the records to Redis. Retrying a partial import validates any existing copy before continuing. The command leaves the old files available for rollback and records completion in Redis to prevent reseeding the old journal later.
4. Start the two new manager pods. Confirm both are Ready and resume an already-started upload from either replica. Back up the new state service as part of routine operations.

To roll back to 0.1.0, finish or cancel open uploads while Redis mode is still available, stop the new managers, and restart 0.1.0 against its retained claim. The old binary cannot read sessions created in Redis mode. Existing S3 objects remain accessible through the CDN.

For a standalone release on one eligible node, set `state.mode: file` and `replicaCount: 1`. The standalone binary defaults to file mode, using `CDN_MANAGER_STATE_DIR` for the journal and staging. Redis mode uses `CDN_MANAGER_STATE_MODE=redis`, `CDN_MANAGER_REDIS_URL`, `CDN_MANAGER_REDIS_PREFIX`, and a private writable `CDN_MANAGER_SPOOL_DIR` per replica. The chart rejects file state with multiple managers. If reusing a volume owned by UID/GID 65532, set `runAsUser`, `runAsGroup`, and `fsGroup` to 65532 until the volume is migrated.
