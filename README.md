# Can I haz Kubernetes?

> **DIGIT 2026 workshop:** Follow the numbered
> [Paws or Claws exercise guide](DIGIT2026.md).

An image board demonstrating [Codemowers Cloud](https://codemowers.cloud/):
direct browser uploads, searchable OCR, object detection, and asynchronous image
processing on Kubernetes.

**Demo:** [can-i-haz-kubernetes.codemowers.io](https://can-i-haz-kubernetes.codemowers.io)

## Architecture

| Service | Language | Role |
|---|---|---|
| uploader | Go | Authorize direct S3 uploads, confirm posts, publish Kafka events |
| browse | Go | Board listing, thread view, comments |
| search | Rust / Rocket | Public title/OCR/tag search in Postgres |
| comments | Go | Post/delete comments |
| admin | Go | Authorize board administration and create/delete boards |
| tagger | Python/YOLO | Kafka consumer → auto-tag images via YOLOv8 |
| ocr | Python/Tesseract | Kafka consumer → extract caption text into image metadata |
| thumbnailer | Python/libjpeg-turbo | Kafka + on-demand JPEG thumbnails cached in S3 |
| exif | Go | Kafka consumer → extract EXIF camera, exposure, dimensions and GPS metadata |
| frontend | Next.js 14 | UI and OIDC session handling |

Traefik in Kubernetes and nginx in local development route
`/api/{browse,search,comments,upload,admin,thumbnails}` directly to the matching backend service
without rewriting the path. API traffic does not pass through the Next.js
process; only `/api/auth` belongs to the frontend.

Images live in S3-compatible storage (`<namespace>-images` in Kubernetes,
`lolcatz-images` in Compose). Browsers transfer image bytes directly using signed
S3 URLs; the public storage endpoint comes from operator-generated settings.
Metadata lives in PostgreSQL (CNPG), board-list caches in Dragonfly, and events
in Redpanda. NextAuth stores sessions, refresh tokens and ID tokens in its
encrypted HttpOnly cookie; only the access token is exposed to browser code.
Postgres enables `pgvector` for the optional InsightFace exercise and PostGIS for EXIF
locations. EXIF coordinates are stored as indexed `geometry(PointZ, 4326)`
for map and proximity queries.

## Run locally

```bash
docker compose up --build
```

Open [localhost:3000](http://localhost:3000). Compose supplies a local development
identity (`developer@localhost`) with upload, comment, and board-admin access.
The MinIO console is at [localhost:9001](http://localhost:9001)
(`minioadmin` / `minioadmin`). Browser uploads require `minio.localhost` to resolve
to `127.0.0.1`; add it to `/etc/hosts` if needed.

Enable the optional YOLO worker with:

```bash
docker compose build tagger-model
docker compose --profile ai up --build
```

Reset disposable local data with `docker compose down -v`.

## Develop on Kubernetes

The [Helm chart](chart/) targets Codemowers Cloud. Inspect the target cluster's
admission policies for platform defaults and requirements; keep those settings
out of application configuration. Namespace lifecycle belongs to the platform.

Copy the Skaffold environment template:

```bash
cp skaffold.env.example skaffold.env
```

Replace the `...` values in `skaffold.env` with your Kubernetes context,
namespace, and default image repository (`SKAFFOLD_DEFAULT_REPO`, for example
`ghcr.io/<your-user>`). This file is ignored by Git and loaded automatically by
Skaffold. Log in to that registry with `docker login`, then start development:

```bash
skaffold dev
```

Open the application through its Ingress hostname. Skaffold port-forwards
are for debugging individual services. Prometheus metrics endpoints use plain
HTTP without TLS.

[Chart values](chart/values.yaml) define image overrides and optional components.
[Sandbox values](chart/values-sandbox.yaml) lower the requests and volume
sizes to fit a small platform-manager sandbox.
[CI](.github/workflows/images.yaml) tests the application and publishes
`ghcr.io/<repository-owner>/lolcatz-<service>` images for `v*` tags, with the
version, commit SHA, and release tags. Main-branch builds are verified without
publishing. The `release-values`
artifact pins image digests and records the source revision; use the chart from
that revision with those values. Public packages need no pull credentials;
private packages require `imagePullSecrets`.

Versioned charts are published to
`oci://ghcr.io/<repository-owner>/charts/lolcatz`; each chart release points to
the matching digest-pinned `v*` images.

## Authentication

Passmower provisions the OIDC client registration; the application uses the
configured OIDC issuer and does not require Passmower. NextAuth owns
authorization-code/PKCE login and renewal, retaining refresh and ID tokens in
its encrypted HttpOnly cookie.
The browser receives the access token and calls each API directly. APIs verify
the signature, issuer, expiry, and public-origin-plus-`/api` audience, then check
operation scopes and ownership.

| Operation | Required scope |
|---|---|
| List own uploads | `lolcatz:images:read` |
| Upload or delete own images | `lolcatz:images:write` |
| Post comments | `lolcatz:comments:write` |
| Manage boards | `lolcatz:boards:write` and `github.com:codemowers:admins` membership |

Browse and search are public. Login links the verified ID-token email and subject
to a local user; API requests resolve ownership from that subject. Client secrets
and long-lived storage credentials stay server-side. New upload clients use
`/api/upload/presign` followed by `/api/upload/confirm`. Send the returned
`put_headers` with the direct S3 PUT: the signature binds the owner and content
type. Confirmation verifies the stored owner and uses the stored content type;
retrying an already confirmed upload does not publish another event.

## Exercises

See [exercises/](exercises/) for image-processing details and exercises.

## Checks

Dockerfiles live in `services/` and use the repository root as their build context.
Go services share [one module](services/go.mod); search uses
[Rust/Rocket](services/search/). Compose provides the databases and dependencies
for integration tests:

```bash
docker compose up --build -d
docker compose run --build --rm frontend-tests
docker compose run --build --rm go-tests
docker compose run --build --rm search-tests
docker compose run --rm node-tools node test/integration.mjs
```

For frontend unit and browser checks, run `npm ci`, `npm test`,
`npx playwright install chromium`, and `npm run test:browser` from
`services/frontend/`. Python worker unit tests run with
`PYTHONPATH=services python3 -m unittest discover -s services/tests` after installing
the worker dependencies. See [CI](.github/workflows/images.yaml) for the complete
check commands.
