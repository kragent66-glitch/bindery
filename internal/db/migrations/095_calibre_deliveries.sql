-- +migrate Up
-- calibre_deliveries is the durable ledger of ebook files sent to Calibre
-- (#2832). Before it, a failed live push was one WARN line and the book was
-- missed for good. One row per ebook book_files row, keyed on book_file_id.
--
-- calibre_id here is the record id in the push TARGET library. It is not
-- books.calibre_id, which keeps meaning the id in the library at
-- calibre.library_path (the import source).
--
-- format is the file extension, lower case and without the dot ('epub').
--
-- Time columns are written by Go in one fixed width UTC shape,
-- 2006-01-02T15:04:05.000000000Z (see calibreDeliveryTime), so that
-- next_attempt_at compares and sorts as text and the (state, next_attempt_at)
-- index serves the due query. RFC3339Nano trims trailing zeros, which breaks
-- text ordering below the second, and CURRENT_TIMESTAMP uses a space
-- separator that sorts before every 'T' on the same day. The DEFAULTs exist
-- only so a hand written row is valid. Every writer passes the value.
CREATE TABLE IF NOT EXISTS calibre_deliveries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id         INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    book_file_id    INTEGER NOT NULL REFERENCES book_files(id) ON DELETE CASCADE,
    edition_id      INTEGER,
    file_path       TEXT    NOT NULL,
    format          TEXT    NOT NULL DEFAULT '',
    state           TEXT    NOT NULL DEFAULT 'pending'
                    CHECK(state IN ('pending','delivered','failed','skipped')),
    outcome         TEXT    NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    last_error_code TEXT    NOT NULL DEFAULT '',
    calibre_id      INTEGER,
    target_library  TEXT    NOT NULL DEFAULT '',
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delivered_at    DATETIME,
    UNIQUE (book_file_id)
);
CREATE INDEX IF NOT EXISTS idx_calibre_deliveries_due  ON calibre_deliveries(state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_calibre_deliveries_book ON calibre_deliveries(book_id);

-- Backfill: a book that already carries a calibre_id and did NOT come from a
-- Calibre library import got that id from a push, so its ebook file is
-- recorded as delivered. Without this, the first "push all" after upgrade
-- would send every one of those books again.
--
-- Calibre origin is excluded three ways. The first two are the test
-- isCalibreOrigin in internal/calibre/syncer.go already applies before it
-- refuses to overwrite a calibre_id (metadata_provider 'calibre', or a
-- foreign_id of calibre:book:N). The third is a book row in
-- calibre_provenance, which the library import writes for EVERY book it
-- matches, including an existing Bindery book it adopted by title and left
-- with its own foreign_id and provider. Such a book's calibre_id is an id in
-- the import library, not proof of a push. Leaving a pushed book out costs a
-- duplicate check on the next push, while recording an unpushed one as
-- delivered would hide it from delivery for good, so doubt excludes.
--
-- The file is the one the book reports as its ebook path
-- (books.ebook_file_path, the order bookColumns reads) and otherwise the
-- lowest id ebook row, the bookCTE fallback. A book with no ebook row gets
-- nothing. ON CONFLICT keeps a re-run from adding or changing anything.
WITH picked AS (
    SELECT b.id AS book_id,
           b.calibre_id AS calibre_id,
           COALESCE(
               (SELECT MIN(x.id) FROM book_files x
                 WHERE x.book_id = b.id AND x.format = 'ebook' AND x.path = b.ebook_file_path),
               (SELECT MIN(x.id) FROM book_files x
                 WHERE x.book_id = b.id AND x.format = 'ebook')
           ) AS book_file_id
    FROM books b
    WHERE b.calibre_id IS NOT NULL AND b.calibre_id > 0
      AND LOWER(TRIM(COALESCE(b.metadata_provider, ''))) <> 'calibre'
      AND LOWER(TRIM(COALESCE(b.foreign_id, ''))) NOT LIKE 'calibre:book:%'
      AND NOT EXISTS (SELECT 1 FROM calibre_provenance p
                       WHERE p.entity_type = 'book' AND p.local_id = b.id)
),
files AS (
    SELECT p.book_id, p.calibre_id, bf.id AS book_file_id, bf.path,
           substr(bf.path, length(rtrim(bf.path, replace(bf.path, '.', ''))) + 1) AS tail
    FROM picked p
    JOIN book_files bf ON bf.id = p.book_file_id
),
stamp AS (
    SELECT strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z' AS ts
)
INSERT INTO calibre_deliveries
    (book_id, book_file_id, file_path, format, state, outcome, attempts,
     calibre_id, next_attempt_at, created_at, updated_at, delivered_at)
SELECT f.book_id, f.book_file_id, f.path,
       CASE WHEN instr(f.path, '.') > 0 AND instr(f.tail, '/') = 0 AND instr(f.tail, char(92)) = 0
            THEN LOWER(f.tail) ELSE '' END,
       'delivered', 'backfilled', 0,
       f.calibre_id, s.ts, s.ts, s.ts, s.ts
FROM files f, stamp s
WHERE 1
ON CONFLICT(book_file_id) DO NOTHING;

-- +migrate Down
DROP TABLE IF EXISTS calibre_deliveries;
