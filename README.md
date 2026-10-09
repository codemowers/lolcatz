# Can I haz Kubernetes?

An image board demonstrating [Codemowers Cloud](https://codemowers.cloud/):
direct browser uploads, searchable OCR, object detection, and asynchronous image
processing on Kubernetes.

**Demo:** [can-i-haz-kubernetes.codemowers.io](https://can-i-haz-kubernetes.codemowers.io)

## Codemowers Cloud sandbox

Obtain a Codemowers Cloud sandbox at [trial.codemowers.io](https://trial.codemowers.io).
Follow the instructions on that site to:

* Install Skaffold.
* Install kubectl.
* Install the OIDC authentication plugin for kubectl.
* Configure your Kubernetes client with the sandbox kubeconfig.
* Set up `skaffold.env` in the project root.
* Refer to [mcp.codemowers.io](https://mcp.codemowers.io) for platform guidance.
* Connect to [mcp.driftmower.aws-us-west-2-bravo.codemowers.io](https://mcp.driftmower.aws-us-west-2-bravo.codemowers.io/).

Proceed to build locally using Docker and deploy to sandbox with:

```
skaffold dev
```

Open the URL in the frontend's startup log: `Lolcatz available at https://…`.
Skaffold streams this log after the application starts.

To fit the sandbox quota, Skaffold leaves admin, EXIF, OCR, the tagger and the
thumbnail worker off. Build and deploy every component with:

```
skaffold dev -p full
```

Once deployed, continue with the [exercises](exercises/), such as real-time
voting, an LLM caption-correction worker, and face similarity search.

## Docker Compose

```bash
docker compose up --build
```

Open [localhost:3000](http://localhost:3000). Compose supplies a local development
identity (`developer@localhost`) with upload, comment, and board-admin access.
Browser uploads require `minio.localhost` to resolve to `127.0.0.1`; add it to
`/etc/hosts` if needed. Enable the optional YOLO worker with
`docker compose build tagger-model && docker compose --profile ai up --build`.
Reset local data with `docker compose down -v`.

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
| frontend | Next.js | UI and OIDC session handling |

Ingress routes `/api/<service>` straight to each backend; only `/api/auth`
belongs to the frontend. Browsers transfer image bytes directly to S3 with
presigned URLs. Metadata lives in PostgreSQL (with PostGIS and pgvector),
caches in Dragonfly, and events in Redpanda. The uploader creates the complete
[schema](services/uploader/schema.sql); workers only write enrichment results.

Optional components are toggled with `admin.enabled`, `exif.enabled`,
`ocr.enabled`, `tagger.enabled` and `thumbnailerWorker.enabled` in the
[chart values](chart/values.yaml). OCR and the tagger default to off; Skaffold's
default profile also turns off admin, EXIF and the thumbnail worker.
`internalTLS.enabled` adds TLS between ingress, application services and
server-side frontend calls; it defaults to off.

## Authentication

Passmower provisions the OIDC client registration. NextAuth owns the
authorization-code/PKCE login and keeps refresh and ID tokens in its encrypted
HttpOnly cookie; the browser calls each API directly with the access token. APIs
verify the token's audience (public origin plus `/api`) and operation scopes:

| Operation | Required scope |
|---|---|
| List own uploads | `lolcatz:images:read` |
| Upload or delete own images | `lolcatz:images:write` |
| Post comments | `lolcatz:comments:write` |
| Manage boards | `lolcatz:boards:write` and `github.com:codemowers:admins` membership |

Browse and search are public. Managing boards requires the admin service
(`admin.enabled`).

## Checks

```bash
docker compose up --build -d
docker compose run --build --rm frontend-tests
docker compose run --build --rm go-tests
docker compose run --build --rm search-tests
docker compose run --rm node-tools node test/integration.mjs
```

[CI](.github/workflows/images.yaml) runs the complete checks and publishes images
and the Helm chart to `ghcr.io/<repository-owner>` on `v*` tags.
