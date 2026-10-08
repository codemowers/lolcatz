-- Complete fresh-install schema, initialized by the core uploader service.
-- Optional workers own enrichment writes, not table creation. The database
-- operator installs PostGIS; creating it here covers superuser dev databases.
CREATE EXTENSION IF NOT EXISTS postgis SCHEMA public;

CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT UNIQUE,
    subject TEXT UNIQUE,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS boards (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9]{1,16}$'),
    name TEXT NOT NULL,
    icon TEXT NOT NULL DEFAULT '📌',
    blurb TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS images (
    id           TEXT PRIMARY KEY,
    board        TEXT NOT NULL DEFAULT 'b' REFERENCES boards(id),
    title        TEXT NOT NULL DEFAULT '',
    filename     TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size         BIGINT NOT NULL DEFAULT 0,
    user_id      BIGINT REFERENCES users(id),
    author       TEXT NOT NULL DEFAULT 'Anonymous',
    user_agent   TEXT NOT NULL DEFAULT '',
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS comments (
    id         BIGSERIAL PRIMARY KEY,
    image_id   TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    user_id    BIGINT REFERENCES users(id),
    author     TEXT NOT NULL DEFAULT 'Anonymous',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO boards (id, name, icon, blurb) SELECT * FROM (VALUES
    ('b', 'Random', '🎲', 'Anything goes'),
    ('ck', 'Food & Cooking', '🍳', 'Good food and kitchen experiments'),
    ('g', 'Technology', '💾', 'Wires and yak shaving'),
    ('k', 'Weapons', '⚔️', 'Pointy things'),
    ('a', 'Anime', '🌸', 'Big eyes, big feelings'),
    ('mu', 'Music', '🎵', 'Loud and otherwise'),
    ('v', 'Video Games', '🎮', 'Backlog denial')
) AS defaults(id,name,icon,blurb) WHERE NOT EXISTS (SELECT 1 FROM boards)
ON CONFLICT (id) DO NOTHING;

CREATE INDEX IF NOT EXISTS comments_image_id ON comments(image_id);
CREATE INDEX IF NOT EXISTS images_board ON images(board, uploaded_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS images_user_id ON images(user_id);
CREATE INDEX IF NOT EXISTS comments_user_id ON comments(user_id);

-- No image FK: deletion events must survive deletion of the image itself.
CREATE TABLE IF NOT EXISTS image_outbox (
    sequence BIGSERIAL PRIMARY KEY,
    image_id TEXT NOT NULL,
    payload BYTEA
);

CREATE TABLE IF NOT EXISTS image_exif (
	image_id         TEXT PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
	captured_at      TIMESTAMPTZ,
	camera_make      TEXT,
	camera_model     TEXT,
	lens_model       TEXT,
	software         TEXT,
	width            INTEGER,
	height           INTEGER,
	orientation      INTEGER,
	iso              INTEGER,
	f_number         REAL,
	exposure_seconds REAL,
	focal_length_mm  REAL,
	location         geometry(PointZ, 4326),
	producer_version TEXT NOT NULL,
	processed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS image_exif_stale ON image_exif(producer_version);
CREATE INDEX IF NOT EXISTS image_exif_location ON image_exif USING GIST(location);

CREATE TABLE IF NOT EXISTS image_ocr (
    image_id         TEXT PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    text             TEXT NOT NULL DEFAULT '',
    language         TEXT,
    derived_title    TEXT,
    producer_version TEXT NOT NULL,
    processed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS image_ocr_stale ON image_ocr(producer_version);

CREATE INDEX IF NOT EXISTS image_ocr_fts
    ON image_ocr USING GIN (to_tsvector('simple', text));

CREATE TABLE IF NOT EXISTS image_annotations (
    id               BIGSERIAL PRIMARY KEY,
    image_id         TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    label            TEXT NOT NULL,
    confidence       REAL NOT NULL,
    x1               REAL NOT NULL CHECK (x1 >= 0 AND x1 <= 1),
    y1               REAL NOT NULL CHECK (y1 >= 0 AND y1 <= 1),
    x2               REAL NOT NULL CHECK (x2 >= 0 AND x2 <= 1),
    y2               REAL NOT NULL CHECK (y2 >= 0 AND y2 <= 1),
    producer_version TEXT NOT NULL,
    processed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS image_annotations_image ON image_annotations(image_id);

CREATE INDEX IF NOT EXISTS image_annotations_label ON image_annotations(label);
