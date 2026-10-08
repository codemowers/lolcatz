# Exercises

## Image processing

Uploads and deletions save keyed events in `image_outbox` in the same PostgreSQL
transaction as the image change. An uploader background publisher delivers them
to `lolcatz-images` in order, deleting pending rows only after Kafka acknowledges
receipt. Outages and restarts retry pending events; a crash after acknowledgement
can duplicate an event, so workers remain idempotent. EXIF, OCR, YOLO, and
thumbnail workers use independent consumer groups and commit offsets after
processing. OCR and YOLO skip corrupt or oversized image inputs while storage
and database failures remain retryable.
YOLO publishes replacement detections to `lolcatz-tags`; deletion tombstones
retire derived data. Image bytes stay in S3 and metadata in PostgreSQL.

To replay a worker after changing its model, stop its consumer group, rewind its
Kafka offsets, and restore its previous replica count. Use the cluster's Kafka
credentials and TLS trust. Bump the producer version to identify stale results;
thumbnail output changes also require a new cache URL/storage version.

## Exercises

- [Paws or Claws](paws-or-claws.md): real-time voting.
- [Caption correction](caption-correction.md): build an LLM worker using
  OCR and YOLO output while preserving the original caption.
- [InsightFace](insight-face.md): consume person detections and implement
  private face embeddings and similarity search. This optional implementation is
  participant work and is excluded from default builds.
