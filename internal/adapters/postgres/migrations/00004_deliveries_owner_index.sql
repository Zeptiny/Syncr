-- +goose Up

-- The delivery log of all of a user's contacts (ListDeliveries without a
-- contact: the web UI's notifications page) reads newest first by owner.
CREATE INDEX deliveries_owner_idx ON deliveries (owner_id, id DESC);

-- +goose Down
DROP INDEX deliveries_owner_idx;
