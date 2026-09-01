CREATE TABLE IF NOT EXISTS targets (
    pk          TEXT PRIMARY KEY,
    username    TEXT    NOT NULL,
    is_private  INTEGER NOT NULL DEFAULT 0,
    resolved_at INTEGER NOT NULL
);

-- Seed from the accounts already being watched. Their ids were resolved on some
-- earlier run and recorded here, so an existing deployment starts with a usable
-- cache instead of having to ask Instagram once more to learn what it knows.
INSERT OR IGNORE INTO targets (pk, username, is_private, resolved_at)
SELECT owner_pk, username, 0, strftime('%s', 'now') FROM watch_state;
