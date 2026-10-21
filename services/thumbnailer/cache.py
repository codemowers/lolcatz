"""Shared, idempotent S3 generation for HTTP requests and Kafka uploads."""
import logging
import os
import time

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from PIL import features
from psycopg2 import OperationalError
from psycopg2.pool import ThreadedConnectionPool

from thumbnail import InvalidImage, SIZES, make_thumbnails, object_key

SIGNED_URL_SECONDS = 7 * 24 * 60 * 60
OBJECT_CACHE_CONTROL = "public, max-age=31536000, immutable"

MAX_BYTES = 100 * 1024 * 1024
log = logging.getLogger(__name__)


class ThumbnailCache:
    def __init__(self):
        if not features.check_feature("libjpeg_turbo"):
            raise RuntimeError("thumbnailer requires libjpeg-turbo")
        for attempt in range(1, 31):
            try:
                self.pool = ThreadedConnectionPool(1, 1, os.environ["DATABASE_URL"], connect_timeout=2)
                break
            except OperationalError as error:
                if attempt == 30:
                    raise
                log.warning("database not ready (attempt %s/30); retrying in 2s: %s", attempt, error)
                time.sleep(2)
        options = dict(
            aws_access_key_id=os.environ["S3_ACCESS_KEY"],
            aws_secret_access_key=os.environ["S3_SECRET_KEY"],
            region_name=os.environ.get("S3_REGION", "us-east-1"),
            config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, connect_timeout=3, read_timeout=15,
                          retries={"max_attempts": 2}),
        )
        scheme = "https" if os.environ["S3_USE_SSL"] == "true" else "http"
        self.s3 = boto3.client("s3", endpoint_url=f"{scheme}://{os.environ['S3_ENDPOINT']}", **options)
        self.bucket = os.environ["S3_BUCKET"]

    def ensure(self, image_id, sizes=SIZES):
        db = self.pool.getconn()
        try:
            with db, db.cursor() as cur:
                # Serialize HTTP/Kafka generation per image, across replicas.
                cur.execute("SELECT pg_advisory_xact_lock(hashtextextended(%s, 0))", (image_id,))
                # Deletion waits until generation finishes, then deletes the cache.
                cur.execute("SELECT board FROM images WHERE id = %s FOR SHARE", (image_id,))
                row = cur.fetchone()
                if row is None:
                    return False
                missing = []
                for size in sizes:
                    try:
                        self.s3.head_object(Bucket=self.bucket, Key=object_key(image_id, size))
                    except ClientError as error:
                        if error.response["Error"]["Code"] not in ("404", "NoSuchKey", "NotFound"):
                            raise
                        missing.append(size)
                if not missing:
                    return True
                started = time.perf_counter()
                obj = self.s3.get_object(Bucket=self.bucket, Key=f"{row[0]}/{image_id}")
                with obj["Body"] as body:
                    if obj["ContentLength"] > MAX_BYTES:
                        raise InvalidImage("image exceeds thumbnail input limit")
                    data = body.read(MAX_BYTES + 1)
                if len(data) > MAX_BYTES:
                    raise InvalidImage("image exceeds thumbnail input limit")
                for size, (encoded, dimensions) in make_thumbnails(data, missing).items():
                    self.s3.put_object(Bucket=self.bucket, Key=object_key(image_id, size),
                                       Body=encoded, ContentType="image/jpeg",
                                       CacheControl=OBJECT_CACHE_CONTROL)
                    log.info("thumbnail %s size=%s bytes=%s dimensions=%s elapsed_ms=%.1f",
                             image_id, size, len(encoded), dimensions,
                             (time.perf_counter() - started) * 1000)
                return True
        finally:
            self.pool.putconn(db)

    def presign(self, image_id, size):
        return self.s3.generate_presigned_url(
            "get_object",
            Params={"Bucket": self.bucket, "Key": object_key(image_id, size),
                    "ResponseCacheControl": OBJECT_CACHE_CONTROL,
                    "ResponseContentType": "image/jpeg"},
            ExpiresIn=SIGNED_URL_SECONDS,
        )

    def delete(self, image_id):
        for size in SIZES:
            self.s3.delete_object(Bucket=self.bucket, Key=object_key(image_id, size))
