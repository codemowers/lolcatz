"""Exercise the worker's real SQL without loading the ML runtime."""
import json
import os
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from tagger import tagger

import psycopg2


@unittest.skipUnless(os.environ.get("TEST_DATABASE_URL"), "TEST_DATABASE_URL is not set")
class PersistenceTest(unittest.TestCase):
    def test_multiple_boxes_and_reprocessing(self):
        detections = [dict(label="person", confidence=.9, x1=.1, y1=.1, x2=.4, y2=.5),
                      dict(label="person", confidence=.8, x1=.5, y1=.5, x2=.9, y2=.9),
                      dict(label="cat", confidence=.7, x1=.2, y1=.2, x2=.3, y2=.3)]
        self.enterContext(patch.object(tagger, "read_image", return_value=None))
        self.enterContext(patch.object(tagger, "detect_annotations", return_value=detections))
        self.enterContext(patch.object(tagger, "PRODUCER_VERSION", "test"))
        db = psycopg2.connect(os.environ["TEST_DATABASE_URL"])
        try:
            with db.cursor() as cur:
                cur.execute("CREATE TEMP TABLE images (id text PRIMARY KEY, board text); INSERT INTO images VALUES ('test', 'b');")
                cur.execute("CREATE TEMP TABLE image_annotations (LIKE public.image_annotations INCLUDING ALL)")
            events = []
            producer = SimpleNamespace(produce=lambda *args, **kwargs: events.append((args, kwargs)))
            message = SimpleNamespace(value=lambda: b'{"id":"test","board":"b"}')
            for _ in range(2):
                tagger.process_message(message, db, None, "test", None, .4, producer, "tags")
                with db.cursor() as cur:
                    cur.execute("SELECT label, confidence FROM image_annotations ORDER BY id")
                    rows = cur.fetchall()
                    self.assertEqual(len(rows), 3)
                    self.assertEqual(rows[0][0], "person")
            self.assertEqual(len(events), 2)
            self.assertTrue(all(args == ("tags",) for args, _ in events))
            self.assertEqual(events[-1][1]["key"], b"test")
            annotations = json.loads(events[-1][1]["value"])
            self.assertEqual(set(annotations), {"person", "cat"})
            self.assertEqual(len(annotations["person"]), 2)
            self.assertEqual(len(annotations["cat"]), 1)
            boxes = [box for group in annotations.values() for box in group]
            self.assertTrue(all(set(box) == {"id", "confidence", "bbox"} for box in boxes))
            with db.cursor() as cur:
                cur.execute("SELECT id FROM image_annotations ORDER BY id")
                self.assertEqual(sorted(box["id"] for box in boxes), [row[0] for row in cur.fetchall()])
            self.assertAlmostEqual(annotations["person"][0]["confidence"], .9)
            for actual, expected in zip(annotations["person"][0]["bbox"], [.1, .1, .4, .5]):
                self.assertAlmostEqual(actual, expected)
            detections.clear()
            tagger.process_message(message, db, None, "test", None, .4, producer, "tags")
            self.assertEqual(json.loads(events[-1][1]["value"]), {})
            self.assertEqual(len(events), 3)
            with db.cursor() as cur:
                cur.execute("SELECT count(*) FROM image_annotations")
                self.assertEqual(cur.fetchone(), (0,))
        finally:
            db.rollback()
            db.close()


if __name__ == "__main__":
    unittest.main()
