-- Schema and initial rows of the end-to-end test, loaded into s1 as the postgres superuser.
-- It covers the cases the replay engine has to get right: identity and generated columns, TOAST,
-- a composite primary key, a table without primary key (REPLICA IDENTITY FULL), a partitioned
-- table, an array of a user-defined enum (copied in text format) and sequences.

CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy');

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text NOT NULL,
    email      text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    profile    jsonb
);

CREATE TABLE orders (
    order_id       serial PRIMARY KEY,
    customer_id    bigint NOT NULL REFERENCES customers (id),
    amount         numeric(12, 2) NOT NULL,
    note           text,
    total_with_tax numeric GENERATED ALWAYS AS (amount * 1.2) STORED,
    tags           mood[]
);
CREATE INDEX orders_customer_idx ON orders (customer_id);

-- Composite primary key.
CREATE TABLE order_items (
    order_id int NOT NULL REFERENCES orders (order_id),
    line_no  int NOT NULL,
    sku      text NOT NULL,
    qty      int NOT NULL,
    PRIMARY KEY (order_id, line_no)
);

-- No primary key: updates and deletes are located by the full old row.
CREATE TABLE events_nopk (
    ts      timestamptz NOT NULL DEFAULT now(),
    kind    text,
    payload json,
    n       int
);
ALTER TABLE events_nopk REPLICA IDENTITY FULL;

CREATE TABLE metrics (
    id     bigserial,
    bucket date NOT NULL,
    val    double precision,
    PRIMARY KEY (id, bucket)
) PARTITION BY RANGE (bucket);
CREATE TABLE metrics_2026_01 PARTITION OF metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE metrics_2026_02 PARTITION OF metrics FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

-- Large values are stored out of line (TOAST); updates that do not touch them must keep them.
CREATE TABLE blobs (
    id   int PRIMARY KEY,
    data bytea,
    doc  text,
    meta int NOT NULL DEFAULT 0
);
-- EXTERNAL = never compressed, always out of line once the row exceeds about 2 kB, so the values
-- below really are TOASTed and an UPDATE of "meta" arrives as an unchanged-TOAST column.
ALTER TABLE blobs ALTER COLUMN data SET STORAGE EXTERNAL;
ALTER TABLE blobs ALTER COLUMN doc SET STORAGE EXTERNAL;

-- A table big enough that the snapshot copy takes a moment, so that writes really happen during it.
CREATE TABLE bulk (
    id      bigserial PRIMARY KEY,
    grp     int NOT NULL,
    payload text
);

INSERT INTO bulk (grp, payload)
SELECT g % 100, md5(g::text) || repeat('x', 100) FROM generate_series(1, 300000) g;

INSERT INTO customers (name, email, profile)
SELECT 'customer ' || g, 'c' || g || '@example.com', jsonb_build_object('n', g, 'tags', jsonb_build_array('a', 'b'))
FROM generate_series(1, 2000) g;

INSERT INTO orders (customer_id, amount, note, tags)
SELECT 1 + (g % 2000), (g % 1000) + 0.5, CASE WHEN g % 10 = 0 THEN NULL ELSE 'note ' || g END,
       CASE WHEN g % 3 = 0 THEN ARRAY['ok', 'happy']::mood[] ELSE NULL END
FROM generate_series(1, 5000) g;

INSERT INTO order_items (order_id, line_no, sku, qty)
SELECT 1 + (g % 5000), 1 + (g / 5000), 'sku-' || (g % 97), 1 + (g % 5)
FROM generate_series(0, 9999) g;

INSERT INTO events_nopk (kind, payload, n)
SELECT 'k' || (g % 7), json_build_object('i', g), CASE WHEN g % 5 = 0 THEN NULL ELSE g % 13 END
FROM generate_series(1, 1500) g;
-- Exact duplicates, to prove that a delete removes a single row.
INSERT INTO events_nopk (ts, kind, payload, n) VALUES
    ('2026-01-01 00:00:00+00', 'dup', '{"x":1}', 1),
    ('2026-01-01 00:00:00+00', 'dup', '{"x":1}', 1),
    ('2026-01-01 00:00:00+00', 'dup', '{"x":1}', 1);

INSERT INTO metrics (bucket, val)
SELECT DATE '2026-01-01' + (g % 59), g * 0.25 FROM generate_series(1, 3000) g;

INSERT INTO blobs (id, data, doc)
SELECT g,
       (SELECT decode(string_agg(md5(random()::text || g || '-' || i), ''), 'hex') FROM generate_series(1, 400) i),
       (SELECT string_agg(md5(random()::text || g || '+' || i), '') FROM generate_series(1, 300) i)
FROM generate_series(1, 20) g;

ANALYZE;
