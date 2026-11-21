-- +goose Up

-- Schedule groups (ADR 0024): named groups of one user's schedules, for
-- organizing the list. A schedule is in at most one group of its own
-- owner: the foreign key on (group_id, owner_id) refuses another owner's
-- group. Nothing that runs schedules reads them. A group goes with its
-- owner (a user who still owns schedules cannot be deleted, so such a
-- group has none left).
CREATE TABLE schedule_groups (
    id          uuid        PRIMARY KEY,
    owner_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    -- The target of schedules' (group_id, owner_id) foreign key.
    UNIQUE (id, owner_id)
);
CREATE UNIQUE INDEX schedule_groups_owner_name_key ON schedule_groups (owner_id, lower(name));

-- DeleteGroup moves a group's schedules out of it first (bumping their
-- version); SET NULL (group_id) is the backstop and keeps owner_id.
ALTER TABLE schedules
    ADD COLUMN group_id uuid,
    ADD CONSTRAINT schedules_group_fkey FOREIGN KEY (group_id, owner_id)
        REFERENCES schedule_groups (id, owner_id) ON DELETE SET NULL (group_id);
-- DeleteGroup's update, and the foreign key's checks.
CREATE INDEX schedules_group_idx ON schedules (group_id) WHERE group_id IS NOT NULL;

-- +goose Down
DROP INDEX schedules_group_idx;
ALTER TABLE schedules DROP CONSTRAINT schedules_group_fkey, DROP COLUMN group_id;
DROP TABLE schedule_groups;
