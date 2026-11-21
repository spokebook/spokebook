-- +goose Up
-- The manufacturer's serial number, e.g. the frame number; empty if unknown.
ALTER TABLE bikes ADD COLUMN serial_number TEXT NOT NULL DEFAULT '';
ALTER TABLE components ADD COLUMN serial_number TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE components DROP COLUMN serial_number;
ALTER TABLE bikes DROP COLUMN serial_number;
