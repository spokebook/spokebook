-- name: CreateNotificationChannel :exec
INSERT INTO notification_channels (id, owner_id, kind, name, config, enabled, last_error, last_sent_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, '', NULL, ?);

-- name: GetNotificationChannel :one
SELECT * FROM notification_channels WHERE owner_id = ? AND id = ?;

-- name: ListNotificationChannels :many
SELECT * FROM notification_channels WHERE owner_id = ? ORDER BY created_at, id;

-- name: UpdateNotificationChannel :execrows
UPDATE notification_channels SET name = ?, config = ?, enabled = ? WHERE owner_id = ? AND id = ?;

-- name: SetNotificationChannelResult :execrows
UPDATE notification_channels
SET last_error = sqlc.arg('last_error'), last_sent_at = coalesce(sqlc.narg('sent_at'), last_sent_at)
WHERE owner_id = sqlc.arg('owner_id') AND id = sqlc.arg('id');

-- name: DeleteNotificationChannel :execrows
DELETE FROM notification_channels WHERE owner_id = ? AND id = ?;

-- The owners with at least one enabled channel and when they were last
-- checked.
-- name: ListNotificationOwners :many
SELECT DISTINCT c.owner_id, s.last_run_at
FROM notification_channels c
LEFT JOIN notification_settings s ON s.owner_id = c.owner_id
WHERE c.enabled = 1
ORDER BY c.owner_id;

-- name: GetNotificationSettings :one
SELECT * FROM notification_settings WHERE owner_id = ?;

-- name: SaveNotificationSettings :exec
INSERT INTO notification_settings (owner_id, events, threshold, open_days, language, last_run_at)
VALUES (?, ?, ?, ?, ?, NULL)
ON CONFLICT (owner_id) DO UPDATE
SET events = excluded.events, threshold = excluded.threshold, open_days = excluded.open_days,
    language = excluded.language;

-- name: SetNotificationLastRun :execrows
UPDATE notification_settings SET last_run_at = ? WHERE owner_id = ?;

-- name: ListNotificationStates :many
SELECT * FROM notification_states WHERE owner_id = ? ORDER BY key;

-- name: SetNotificationState :exec
INSERT INTO notification_states (owner_id, key, state, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (owner_id, key) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at;

-- name: DeleteNotificationState :exec
DELETE FROM notification_states WHERE owner_id = ? AND key = ?;

-- name: DeleteNotificationStates :exec
DELETE FROM notification_states WHERE owner_id = ?;
