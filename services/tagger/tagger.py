"""
Image tagger — consumes lolcatz-images Kafka topic, runs YOLO on each image,
stores detections in Postgres and publishes all annotations to lolcatz-tags.
"""

import json
import logging
import os
from image_input import InvalidImage, decode_image, read_bytes

# Recorded on every row written. Bump it (or set PRODUCER_VERSION) when the
# model changes, so stale rows can be found and replayed selectively.
PRODUCER_VERSION = os.environ.get("PRODUCER_VERSION", "yolov8n-1")

log = logging.getLogger(__name__)


def must_env(k: str) -> str:
    v = os.environ.get(k)
    if not v:
        raise RuntimeError(f"required env var {k} is not set")
    return v


def get_env(k: str, default: str) -> str:
    return os.environ.get(k, default)


def read_image(s3, bucket: str, object_key: str):
    return decode_image(read_bytes(s3, bucket, object_key))


def detect_annotations(model, image, confidence: float) -> list[dict]:
    """Return every accepted normalized detection, regardless of class."""
    results = model(image, verbose=False)
    detections: list[dict] = []
    for result in results:
        for box in result.boxes:
            score = float(box.conf)
            if score < confidence:
                continue
            label = model.names[int(box.cls)]
            x1, y1, x2, y2 = (float(value) for value in box.xyxyn[0].tolist())
            detections.append({
                "label": label,
                "confidence": score,
                "x1": x1,
                "y1": y1,
                "x2": x2,
                "y2": y2,
            })
    return detections


def process_message(message, db, s3, bucket, model, confidence, producer, topic_out):
    payload = message.value()
    # Compacted-topic tombstones retire deleted images. Derived rows are
    # already removed by the image foreign key's ON DELETE CASCADE.
    if payload is None:
        key = message.key()
        if key is None:
            raise ValueError("image tombstone has no key")
        producer.produce(topic_out, key=key, value=None)
        return
    event = json.loads(payload)
    image_id = event["id"]
    with db.cursor() as cur:
        cur.execute("SELECT board FROM images WHERE id = %s LIMIT 1", (image_id,))
        image_row = cur.fetchone()
    if image_row is None:
        return
    object_key = f"{image_row[0]}/{image_id}"
    try:
        image = read_image(s3, bucket, object_key)
    except InvalidImage:
        log.warning("skipping invalid tagger image %s", image_id, exc_info=True)
        return
    detections = detect_annotations(model, image, confidence)

    # Keep every above-threshold detection. Browse/search derive a compact tag
    # list from the highest-confidence annotation for each label.
    with db, db.cursor() as cur:
        cur.execute("DELETE FROM image_annotations WHERE image_id = %s", (image_id,))
        if detections:
            cur.executemany(
                """INSERT INTO image_annotations
                   (image_id, label, confidence, x1, y1, x2, y2, producer_version)
                   VALUES (%(image_id)s, %(label)s, %(confidence)s, %(x1)s, %(y1)s, %(x2)s, %(y2)s, %(producer_version)s)""",
                [dict(detection, image_id=image_id, producer_version=PRODUCER_VERSION)
                 for detection in detections],
            )
        cur.execute(
            """SELECT id, label, confidence, x1, y1, x2, y2
               FROM image_annotations WHERE image_id = %s
               ORDER BY id""",
            (image_id,),
        )
        annotations = {}
        for row in cur.fetchall():
            annotations.setdefault(row[1], []).append({
                "id": row[0], "confidence": row[2],
                "bbox": [row[3], row[4], row[5], row[6]],
            })

    producer.produce(
        topic_out,
        key=image_id.encode(),
        value=json.dumps(annotations).encode(),
    )


def main():
    import psycopg2
    import boto3
    from botocore.client import Config
    from worker_runtime import consume, kafka_security, PublishError
    from confluent_kafka import Consumer, Producer
    from ultralytics import YOLO

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    model = YOLO(get_env("YOLO_MODEL_PATH", "yolov8n.pt"))

    db = psycopg2.connect(must_env("DATABASE_URL"))
    db.autocommit = True
    endpoint = must_env("S3_ENDPOINT")
    scheme = "https" if os.environ["S3_USE_SSL"] == "true" else "http"
    endpoint = f"{scheme}://{endpoint}"
    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=must_env("S3_ACCESS_KEY"),
        aws_secret_access_key=must_env("S3_SECRET_KEY"),
        config=Config(signature_version="s3v4", connect_timeout=3, read_timeout=15, retries={"max_attempts": 2}),
        region_name=get_env("S3_REGION", "us-east-1"),
    )
    bucket = must_env("S3_BUCKET")
    brokers = must_env("KAFKA_BROKERS")
    topic_in = get_env("KAFKA_TOPIC_IN", "lolcatz-images")
    topic_out = get_env("KAFKA_TOPIC_OUT", "lolcatz-tags")
    conf_threshold = float(get_env("YOLO_CONF_THRESHOLD", "0.4"))

    consumer = Consumer({
        **kafka_security(),
        "bootstrap.servers": brokers,
        "group.id": "lolcatz-tagger",
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
        "enable.auto.offset.store": False,
        "max.poll.interval.ms": 900000,
    })
    consumer.subscribe([topic_in])

    delivery_errors = []
    producer = Producer({
        **kafka_security(),
        "bootstrap.servers": brokers,
        "delivery.timeout.ms": 10000,
        "on_delivery": lambda error, _message: delivery_errors.append(error) if error else None,
    })

    log.info("listening on %s", topic_in)
    def process_and_publish(message):
        delivery_errors.clear()
        process_message(message, db, s3, bucket, model, conf_threshold, producer,
                        topic_out)
        if producer.flush(15) or delivery_errors:
            raise PublishError("Kafka output delivery failed")

    try:
        consume(consumer, process_and_publish)
    finally:
        producer.flush(15)
        db.close()


if __name__ == "__main__":
    main()
