# Local end-to-end acceptance

Run the demo with Docker Compose and open [CDN Manager](http://127.0.0.1:8080).

```sh
docker compose up -d --build --wait
```

Choose **Demo Owner** on the single sign-on page, upload a file to **Assets**, and use **CDN link** on its row. Open the copied URL in a private browser window to read it through the local CDN. **Media** uses resumable multipart upload and a separate NGINX gateway at the `/media/` prefix. Garage and the signing gateways are private to the Compose network. The app, demo identity provider, and CDN have loopback bindings. All demo accounts and keys are fixtures.

Run the real browser, REST, S3, and MCP scenario from the repository root. You need Bun 1.4.2 or later, FFmpeg built with libx264, and permission to restart the demo Compose service.

```sh
cd web
bun install --frozen-lockfile
bun x playwright install --with-deps chromium
CDN_TEST_BASE_URL=http://127.0.0.1:8080 \
CDN_TEST_SCREENSHOTS=../e2e/artifacts \
bun run test:browser tests/compose.spec.ts
```

The scenario generates a valid 72 MiB MP4, checks the manager container's 64 MiB memory limit, drops one part request, and restarts the manager to test recovery from disk and Garage. It verifies the CDN availability action, compares the downloaded video with the fixture by SHA-256, checks anonymous CDN range requests with the manager stopped, and measures gateway ingress for the first range. It also compares the browser clipboard with the MCP result and checks the Prometheus scrape after real transfers. Setting `CDN_TEST_SCREENSHOTS` saves the browser images, transfer measurements, and Prometheus scrape together. OAuth uses signed tokens and real authorization-code exchanges with the bundled demo issuer. Production Authentik, network paths between clusters, and the deployed CDN require a separate run.

Run `bun run test:browser tests/manager.spec.ts` to check browser behavior using fixture API responses, including text conflict recovery. Garage configuration in Compose leaves conditional changes disabled. Run `docker compose down` to stop the demo and keep its storage volumes for another run.
