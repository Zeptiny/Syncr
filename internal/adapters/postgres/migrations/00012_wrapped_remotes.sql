-- +goose Up

-- Wrapper remotes (remotes.Remote.Wrapped; the crypt type, ADR 0021) store
-- their data in a folder of one other remote: wrapped_remote_id is that
-- remote and wrapped_path the folder on it, relative to its root ('' is its
-- root). Both are unset for every other type. Remotes are only
-- soft-deleted, so the foreign key keeps pointing at a row; the remotes use
-- case and DeleteRemote (ListRemotesUsing, in the delete transaction)
-- refuse to delete a remote that a live wrapper uses. Rows from before
-- this migration get no wrapped remote.
ALTER TABLE remotes
    ADD COLUMN wrapped_remote_id uuid REFERENCES remotes (id),
    ADD COLUMN wrapped_path text NOT NULL DEFAULT '',
    ADD CONSTRAINT remotes_wrapped_not_self CHECK (wrapped_remote_id IS NULL OR wrapped_remote_id <> id),
    ADD CONSTRAINT remotes_wrapped_path CHECK (wrapped_remote_id IS NOT NULL OR wrapped_path = '');

-- The remotes that wrap a remote (ListRemotesUsing).
CREATE INDEX remotes_wrapped_remote_idx ON remotes (wrapped_remote_id) WHERE wrapped_remote_id IS NOT NULL;

-- +goose Down
DROP INDEX remotes_wrapped_remote_idx;
ALTER TABLE remotes
    DROP CONSTRAINT remotes_wrapped_path,
    DROP CONSTRAINT remotes_wrapped_not_self,
    DROP COLUMN wrapped_path,
    DROP COLUMN wrapped_remote_id;
