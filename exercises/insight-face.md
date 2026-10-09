# InsightFace

Build an optional Python worker that turns YOLO person annotations into private
face embeddings and provides similarity search over those annotations.

## Goal

Consume person detections, fetch the original image, crop each detection in
memory, and use InsightFace to extract a normalized face embedding. Store the
vectors in PostgreSQL with pgvector. Return matching image and annotation IDs
with similarity scores through an internal HTTP endpoint.

Do not write crops to S3 or return raw embeddings in browse, search or similarity
responses. A person detection does not guarantee a visible face: skip crops
without a detected face. This exercise works with human faces, not cat faces.

## Provided integration

```text
Upload → lolcatz-images → YOLO tagger → image_annotations
                              │
                              └→ lolcatz-tags → your worker
                                                       │
                                Original image in S3 ──┤
                                                       ▼
                                           PostgreSQL / pgvector
                                                       ▲
                                           Internal similarity API
```

The application already provides:

- The frontend can draw all YOLO bounding boxes with labels and confidence
  scores on board cards, search results and thread images.
- `services/uploader/schema.sql`: one `image_annotations` row per detection, with
  a numeric ID, image ID, label, confidence, normalized coordinates and producer
  version. Browse/search aggregate these rows into public tags.
- `services/tagger/tagger.py`: publishes all annotations after storing them.
  `KAFKA_TOPIC_OUT` defaults to `lolcatz-tags`; each consumer filters its classes.
- `chart/templates/tags-topic.yaml`: a compacted topic keyed by image ID.
  Compose also creates this topic for local tagger runs.
- The Kubernetes PostgreSQL cluster and Database resources enable pgvector.
- `insightface.image`, `insightfaceModel.image` and `insightface.enabled` Helm
  values for wiring in the completed worker. pgvector is already enabled;
  implementing the exercise does not require changing the database extensions.

The source directory `services/insightface/` and Helm templates matching
`chart/templates/insightface-*.yaml` are Git-ignored participant work. They may
exist in an instructor's working directory but are absent from a fresh clone.
The default build and CI do not require them. Keep the shared topic, annotation
schema and database extension in the base application.

## Event contract

Consume `lolcatz-tags` using an independent group, `lolcatz-insightface`:

```json
{
  "person": [
    {
      "id": 42,
      "confidence": 0.93,
      "bbox": [0.1, 0.05, 0.8, 0.95]
    }
  ],
  "cat": [
    {
      "id": 43,
      "confidence": 0.88,
      "bbox": [0.4, 0.3, 0.7, 0.8]
    }
  ]
}
```

The Kafka key is the image ID; read it from the record key, not the value.
Read `detections.get("person", [])` inside your worker; keys use the original
YOLO class names, not pluralized names. `bbox` is `[x1, y1, x2, y2]` in the range
`[0, 1]`, relative to the EXIF-oriented image. Resolve the current board from
`images` and fetch the S3 object at `{board}/{image_id}`. Apply EXIF orientation
before interpreting coordinates, just as the tagger does.

The tagger replaces annotations when reprocessing an image, so annotation IDs
can change. Validate that each supplied annotation still belongs to the image;
skip deleted images and obsolete IDs. Every successful run publishes its whole
annotation object, including `{}` when nothing was detected. Deletions produce
Kafka tombstones (null record values). A run with no person detections requires
no face inference. Do not use Kafka retention as proof that a database row is
still current.

## Participant tasks

1. Create `services/insightface/` with a Python worker, dependency file, runtime
   `services/Dockerfile.insightface` and separate
   `services/Dockerfile.insightface-model` for the model weights.
2. Load the `buffalo_l` model from the mounted model image. Use CPU inference
   initially. Decode the image once, clamp bounding boxes, reject empty crops,
   and convert RGB to the input format expected by the model. Select the
   highest-confidence detected face in each person crop.
3. Create a service-owned `face_embeddings` table with:
   - `annotation_id`: primary key referencing `image_annotations(id)` with
     `ON DELETE CASCADE`;
   - `embedding`: `vector(512)`;
   - `model`: the model/version identifier; and
   - `created_at`: a timestamp.
4. Add a model index and an HNSW index using `vector_cosine_ops`. Upsert by
   annotation ID so replaying a current event does not create duplicates.
   Remove an old embedding if reprocessing its crop no longer finds a face.
5. Handle malformed events and Kafka tombstones explicitly. Roll back failed
   database transactions, and commit consumer offsets only after successful
   processing or an intentional skip. Retry transient storage/database errors.
6. Implement the internal similarity endpoint below, filtering comparisons by
   model and excluding the queried annotation.
7. Create `chart/templates/insightface-deployment.yaml` and
   `chart/templates/insightface-service.yaml`. Wrap both templates in
   `{{- if .Values.insightface.enabled }}` / `{{- end }}` so the default chart
   remains usable with your local files present.
8. Register both images in the default Skaffold build as described below and
   set `insightface.enabled: true` in `chart/values.yaml`. Then run
   `skaffold dev -p full`, which also deploys the tagger, and verify processing,
   replay and similarity search using your test images.

## Similarity API

Expose port 8080 through the internal Service `lolcatz-insightface`:

```http
POST /api/insightface/similar
Content-Type: application/json

{"annotation_id": 42, "limit": 20}
```

Return:

```json
{
  "results": [
    {"image_id": "01JDEF456", "annotation_id": 87, "label": "person", "similarity": 0.86}
  ]
}
```

Compute similarity as `1 - cosine_distance` using pgvector's `<=>` operator,
and order by ascending distance. The score is not a probability. Default the
limit to 20 and bound it to 1–100. Return HTTP 400 for invalid input and an empty
results list when the requested annotation has no embedding. Never serialize
the vector itself. The base frontend and Ingress do not expose this endpoint;
use a port-forward for the exercise.

## Deployment configuration

Follow the existing tagger Deployment for secret references, image pull secrets,
resource limits and the read-only root filesystem. Configure the worker with:

| Variable | Value or source |
|---|---|
| `DATABASE_URL` | `lolcatz-database-app` Secret, key `uri` |
| `PGSSLMODE` | `verify-full` |
| `KAFKA_BROKERS` | `lolcatz-redpanda.{{ .Release.Namespace }}.svc.cluster.local:9093` |
| `KAFKA_TLS` | `true` |
| `KAFKA_USERNAME` | `lolcatz-insightface` |
| `KAFKA_PASSWORD` | `lolcatz-insightface-kafka` Secret, key `password` |
| `KAFKA_TOPIC` | `lolcatz-tags` |
| `KAFKA_GROUP` | `lolcatz-insightface` |
| `S3_REGION` | `lolcatz-storage` Secret, key `region` |
| `S3_ENDPOINT` | `lolcatz-storage` Secret, key `publicEndpoint` |
| `S3_USE_SSL` | `true` |
| `S3_BUCKET` | `{{ .Release.Namespace }}.lolcatz` |
| `S3_ACCESS_KEY` | `lolcatz-storage` Secret, key `accessKey` |
| `S3_SECRET_KEY` | `lolcatz-storage` Secret, key `secretKey` |
| `INSIGHTFACE_MODEL` | `buffalo_l` |
| `INSIGHTFACE_ROOT` | `/` when weights are at `/models/buffalo_l/` |
| `ORT_PROVIDER` | `CPUExecutionProvider` |
| `MPLCONFIGDIR` | `/tmp/matplotlib` |
| `PORT` | `8080` |

Add `insightface` to the Redpanda users in `chart/templates/kafka-users.yaml`;
its `insightface.enabled` guard keeps the user out of the default chart.

Use `.Values.insightface.image` for the worker and
`.Values.insightfaceModel.image` for the model image volume. Put `buffalo_l/` at
the model image's root and mount that image at `/models`, read-only. Follow
`services/Dockerfile.tagger-model`: download and extract the weights in a curl
builder stage, then copy only the model directories into `FROM scratch AS final`.
Prepackage the weights so startup does not need a download. Provide a writable `emptyDir`
at `/tmp`. Start with a 1 GiB memory request and 3 GiB limit, then measure usage.

Once the local source and templates exist, add these entries to the existing
`build.artifacts` list in `skaffold.yaml` (Skaffold prefixes the names with
`SKAFFOLD_DEFAULT_REPO` from `skaffold.env`):

```yaml
- image: lolcatz-insightface
  docker:
    dockerfile: services/Dockerfile.insightface
- image: lolcatz-insightface-model
  docker:
    dockerfile: services/Dockerfile.insightface-model
```

Add the corresponding entries under the existing Helm release's
`setValueTemplates`, matching the image names above:

```yaml
insightface.image: "{{.IMAGE_FULLY_QUALIFIED_lolcatz_insightface}}"
insightfaceModel.image: "{{.IMAGE_FULLY_QUALIFIED_lolcatz_insightface_model}}"
```

With `insightface.enabled: true` in `chart/values.yaml`, `skaffold dev -p full`
builds and deploys the completed exercise along with the tagger it consumes.
A small sandbox's quota (4Gi memory requests, 8Gi limits) does not fit this
worker on top of `-p full`; on your branch, drop the `full` profile patches that
enable admin, EXIF, OCR and the thumbnail worker.

```sh
# Validate the chart including your templates.
helm template lolcatz chart

# Build the base application, the tagger, the worker and model.
skaffold dev -p full

# In another terminal, in the sandbox context and namespace:
kubectl port-forward svc/lolcatz-insightface 8085:8080
curl http://localhost:8085/api/insightface/similar \
  -H 'Content-Type: application/json' \
  -d '{"annotation_id":42,"limit":20}'
```

The base Compose PostgreSQL image provides PostGIS but does not provision
pgvector. For a fully local implementation, supply a PostgreSQL image with both
extensions and a Compose override for the worker. The Kubernetes setup already
provides both extensions. In that override, give the model build service a
`build` profile, an explicit image name such as `lolcatz-insightface-model:local`,
and `build.target: final`. Build it explicitly before starting the worker, and
mount that image using a read-only `type: image` volume at `/models`, following
the tagger's Compose configuration. The model image contains no executable and
must not be started as an initializer container.

Keep exercise builds out of the default CI pipeline
while their source remains ignored.

## Acceptance criteria

- The default application builds and renders without any exercise source files.
- After registering the completed implementation, `skaffold dev -p full` builds
  both exercise images alongside the tagger and base services.
- A visible face produces one normalized 512-dimensional vector; a crop without
  a face produces none.
- Processing the same current event twice leaves one row per annotation.
- Deleted images and stale annotation IDs do not cause foreign-key errors or
  recreate embeddings; deleting an annotation cascades to its embedding.
- Similarity results exclude the query annotation and other model versions,
  respect the limit, and contain IDs and scores without vectors.
- Original S3 objects are unchanged and no crop objects are created.
- Restarting the worker preserves results, and replaying its consumer group
  rebuilds embeddings for current annotations. Replaying the tagger first
  generates fresh jobs when the annotation set needs rebuilding.

For replay commands, follow the [replay procedure](README.md#image-processing)
using `lolcatz-insightface` as both Deployment name and consumer group.
