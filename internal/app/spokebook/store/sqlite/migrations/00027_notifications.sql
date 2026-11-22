-- +goose Up
-- A way to notify an owner, e.g. an ntfy topic.
CREATE TABLE notification_channels (
    id           TEXT PRIMARY KEY,
    owner_id     TEXT NOT NULL,
    -- The kind of channel, e.g. 'ntfy'.
    kind         TEXT NOT NULL CHECK (kind <> ''),
    name         TEXT NOT NULL CHECK (name <> ''),
    -- The whole configuration as JSON, sealed with APP_SECRET_KEY (package
    -- secret), never in plain text: an ntfy topic works like a password.
    config       BLOB NOT NULL,
    enabled      INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    -- Why the last sending failed; '' if it didn't.
    last_error   TEXT NOT NULL,
    last_sent_at TEXT,
    created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX notification_channels_owner_idx ON notification_channels (owner_id, created_at);

-- What an owner wants to be notified about; no row means the defaults.
CREATE TABLE notification_settings (
    owner_id    TEXT PRIMARY KEY,
    -- The enabled events, separated by spaces, e.g. 'maintenance wear'.
    events      TEXT NOT NULL,
    -- From which state on items count: 'soon' or 'overdue'.
    threshold   TEXT NOT NULL CHECK (threshold IN ('soon', 'overdue')),
    -- After how many days an open maintenance is notified.
    open_days   INTEGER NOT NULL CHECK (open_days > 0),
    -- The language of the messages, e.g. 'en'.
    language    TEXT NOT NULL,
    last_run_at TEXT
) STRICT;

-- The state of each condition last notified (e.g. 'level:<bike>:minor' is
-- 'soon'), so each state is notified once. A row goes when its condition
-- is gone, so it is notified again when it comes back.
CREATE TABLE notification_states (
    owner_id   TEXT NOT NULL,
    key        TEXT NOT NULL,
    state      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (owner_id, key)
) STRICT;

-- +goose Down
DROP TABLE notification_states;
DROP TABLE notification_settings;
DROP TABLE notification_channels;
