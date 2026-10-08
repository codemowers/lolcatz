"""OCR worker for captioned lolcat images."""

import io
import json
import logging
import os
import re
import subprocess
from image_input import InvalidImage, decode_image, read_bytes

# Recorded on every row written; bump when extraction changes.
PRODUCER_VERSION = os.environ.get("PRODUCER_VERSION", "tesseract-1")

log = logging.getLogger(__name__)


def env(name: str, default: str | None = None) -> str:
    value = os.getenv(name, default)
    if not value:
        raise RuntimeError(f"required env var {name} is not set")
    return value


def sanitize_text(value: str) -> str:
    # Keep readable whitespace, remove terminal/control escapes, and bound DB/UI payloads.
    value = re.sub(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]", "", value)
    value = re.sub(r"[ \t]+", " ", value)
    value = re.sub(r"\n{3,}", "\n\n", value)
    return value.strip()[:20000]


def title_from_ocr(text: str) -> str:
    for line in text.splitlines():
        line = " ".join(line.split()).strip(" -–—")
        if line:
            return line[:160]
    return ""


def extract_text(image_bytes: bytes, language: str) -> str:
    with decode_image(image_bytes) as image:
        encoded = io.BytesIO()
        image.save(encoded, format="PNG")

    result = subprocess.run(
        ["tesseract", "stdin", "stdout", "-l", language],
        input=encoded.getvalue(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=True,
        timeout=45,
    )
    return sanitize_text(result.stdout.decode("utf-8", errors="replace"))


def process_message(message, db, s3, bucket: str, language: str) -> None:
    payload = message.value()
    # Compacted-topic tombstones retire deleted images. Derived rows are
    # already removed by the image foreign key's ON DELETE CASCADE.
    if payload is None:
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
        image_bytes = read_bytes(s3, bucket, object_key)
        text = extract_text(image_bytes, language)
    except (InvalidImage, subprocess.TimeoutExpired):
        log.warning("skipping invalid or over-budget OCR image %s", image_id, exc_info=True)
        return

    # derived_title stays in this service's own table; browse falls back to it
    # when the uploader's title is blank. Writing images.title from here would
    # mean two services owning one column.
    with db.cursor() as cur:
        cur.execute(
            """INSERT INTO image_ocr (image_id, text, language, derived_title, producer_version)
               VALUES (%s, %s, %s, %s, %s)
               ON CONFLICT (image_id) DO UPDATE SET
                   text = EXCLUDED.text,
                   language = EXCLUDED.language,
                   derived_title = EXCLUDED.derived_title,
                   producer_version = EXCLUDED.producer_version,
                   processed_at = NOW()""",
            (image_id, text, language, title_from_ocr(text) or None, PRODUCER_VERSION),
        )


def main():
    import boto3
    import psycopg2
    from botocore.client import Config
    from worker_runtime import consume, kafka_security
    from confluent_kafka import Consumer

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    db = psycopg2.connect(env("DATABASE_URL"))
    db.autocommit = True

    endpoint = env("S3_ENDPOINT")
    scheme = "https" if os.environ["S3_USE_SSL"] == "true" else "http"
    endpoint = f"{scheme}://{endpoint}"
    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=env("S3_ACCESS_KEY"),
        aws_secret_access_key=env("S3_SECRET_KEY"),
        config=Config(signature_version="s3v4", connect_timeout=3, read_timeout=15, retries={"max_attempts": 2}),
        region_name=env("S3_REGION", "us-east-1"),
    )
    bucket = env("S3_BUCKET")
    language = env("OCR_LANGUAGE", "eng")
    consumer = Consumer({
        **kafka_security(),
        "bootstrap.servers": env("KAFKA_BROKERS"),
        "group.id": env("OCR_GROUP", "lolcatz-ocr"),
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
        "enable.auto.offset.store": False,
        "max.poll.interval.ms": 900000,
    })
    consumer.subscribe([env("KAFKA_TOPIC", "lolcatz-images")])
    log.info("listening for OCR jobs using tesseract language %s", language)

    try:
        consume(consumer, lambda message: process_message(message, db, s3, bucket, language))
    finally:
        db.close()


if __name__ == "__main__":
    main()
