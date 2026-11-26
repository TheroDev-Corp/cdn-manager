# CDN Manager

Manage files across S3-compatible sources and share permanent CDN links. The browser app uses OIDC, and an OAuth-protected MCP endpoint exposes the same authorized sources and permanent links. An independently deployed gateway serves CDN requests from storage.

## Try it locally

Install Docker with the Compose plugin and start the example from a clean checkout.

```sh
docker compose up -d --build --wait
```

1. Open [the manager](http://127.0.0.1:8080/) and choose **Continue with single sign-on**, then **Demo Owner**.
2. Select **Assets** or **Media** and upload a file. Open its details to copy the permanent CDN URL or check availability from the manager.
3. Open the URL in another browser session. The example CDN is at [127.0.0.1:8081](http://127.0.0.1:8081/), backed by the two private Garage buckets.

The Compose example uses a loopback-only demo identity provider, bundled test credentials, and persistent Docker volumes. Stop it with `docker compose stop`. The upload controls resume unfinished multipart uploads when you reselect the original file.

## Configure a deployment

Set `CDN_MANAGER_CONFIG` to a JSON file. There is a complete [Compose configuration example](e2e/demo/config.json) and equivalent structure in the [chart values](charts/cdn-manager/values.yaml).

| Setting | Meaning |
| --- | --- |
| `auth.publicUrl` | External HTTPS origin of the manager. Register this origin with `/auth/callback` appended as the web OAuth redirect URI. |
| `auth.oidcIssuer`, `oidcClientId`, `oidcClientSecretEnv` | Web OIDC application and the environment variable containing its confidential client secret. The ID token includes the user's `groups` claim. |
| `auth.mcpIssuer`, `mcpAudience`, `mcpScopes` | MCP token issuer, a dedicated audience distinct from the web client ID, and allowed `read`, `write`, and `delete` scopes. |
| `sources[].id`, `endpoint`, `region`, `pathStyle`, `buckets` | Stable source ID, provider endpoint and signing configuration, and explicit bucket allowlist. |
| `sources[].accessKeyEnv`, `secretKeyEnv`, `sessionTokenEnv` | Environment variable names for S3 credentials. The session token is optional. |
| `sources[].roles` | Identity-provider groups mapped to `viewer`, `editor`, and `admin` on the source. |
| `sources[].capabilities` | Provider features with explicit opt-in. Use `multipart`, `conditionalWrites`, `versions`, `bucketManagement`, `bucketPolicies`, and `presign` only where supported. |
| `sources[].cdnUrl` | Permanent link template containing exactly one `{key}` in its path. Use `{bucket}` if each bucket has its own route. |
| `sources[].cdnProbeUrl` | Optional gateway template reachable from the manager. By default the availability check uses `cdnUrl` itself. |
| `sources[].listingEncoding` | List response decoding. The default, `path`, preserves literal plus signs. SeaweedFS 4.55 uses `form` for keys that encode spaces as plus signs. |

Viewers read, download, and generate permanent links. Editors also upload and edit text. Admins have delete, presign, and bucket administration permissions where the provider supports them. MCP sessions additionally require the scope for each requested operation. See the [Authentik and MCP setup](docs/mcp-authentik.md) for OAuth configuration and a client example.

The web and MCP listener defaults to `:8080`. The internal listener defaults to `:9090` and serves `/livez`, `/readyz`, `/metrics`, and `/sources/health`. Set `CDN_MANAGER_LISTEN` and `CDN_MANAGER_INTERNAL_LISTEN` to change these addresses. Mount durable writable storage at `CDN_MANAGER_STATE_DIR` for the multipart journal and bounded part staging. One process owns a state directory at a time. [REST OpenAPI contract](docs/openapi.yaml)

The gateway must route each CDN template to its configured source and bucket. Keep origin credentials at the gateway and strip any routing prefix before signing its S3 request. Test public read and Range support from the recipient's network independently of the manager. Presigned S3 URLs require the recipient to reach the configured S3 endpoint.

## Install the Helm chart

Create the namespace and an existing Secret with the environment variable names from your deployment values. This example uses an env file held outside the checkout.

```sh
kubectl create namespace cdn-manager
kubectl -n cdn-manager create secret generic cdn-manager-credentials \
  --from-env-file=/secure/path/cdn-manager.env

helm upgrade --install cdn-manager oci://ghcr.io/therodev-corp/charts/cdn-manager \
  --version 0.1.0 --namespace cdn-manager --values /secure/path/values.yaml
```

To render from a checkout, replace the OCI chart URL with `./charts/cdn-manager` and omit `--version`. Set `existingSecret` to the Secret name, configure the identity provider and sources, and supply a TLS Secret if enabling managed Ingress. Configure the ingress controller to allow each multipart chunk and to stream ZIP and download responses. Ensure the configured storage class supports the pod's `fsGroup`. The chart creates a retained PVC by default and supports `persistence.existingClaim` for an existing volume. Upgrades replace one manager pod at a time and preserve the upload journal.

Enable `serviceMonitor.enabled` when the monitoring stack supports the ServiceMonitor CRD. Import the [API, SSO and MCP dashboard](observability/grafana/api.json) and [source transfer dashboard](observability/grafana/transfers.json) and select a Prometheus-compatible datasource. Source health refreshes independently of manager readiness. Managed sources check bucket listing, so a name reserved for future creation can remain in the allowlist. Fixed sources check access to each allowlisted bucket. Public gateway cache and playback metrics come from the gateway.

## Build and test

```sh
(cd web && bun install --frozen-lockfile && bun run build)
go vet ./...
go test -race ./...
helm lint charts/cdn-manager
helm template cdn-manager charts/cdn-manager
```

The web build must precede the Go build because the binary embeds the generated assets. For the live browser test, start the Compose fixture and install Chromium and FFmpeg.

```sh
cd web
bunx playwright install --with-deps chromium
bun run test:browser tests/manager.spec.ts
CDN_TEST_BASE_URL=http://127.0.0.1:8080 bun run test:browser tests/compose.spec.ts --workers=1
```

See the [provider compatibility matrix](docs/compatibility.md) for exercised behavior, required fixture environment variables, and parity gaps. The release workflow validates the matching chart and application versions and publishes the tagged image for Linux amd64 and arm64 alongside a versioned OCI chart.

Licensed under [Apache-2.0](LICENSE).
