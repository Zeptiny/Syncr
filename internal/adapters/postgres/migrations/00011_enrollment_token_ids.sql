-- +goose Up

-- Enrollment tokens get an ID (workers.EnrollmentToken.ID), so the admin who
-- created one can follow the enrollment it starts (GetEnrollmentToken)
-- without handing the token itself back. Tokens created before this column
-- get a random ID; nothing refers to them by it.
ALTER TABLE enrollment_tokens ADD COLUMN id uuid UNIQUE;
UPDATE enrollment_tokens SET id = gen_random_uuid();
ALTER TABLE enrollment_tokens ALTER COLUMN id SET NOT NULL;

-- +goose Down
ALTER TABLE enrollment_tokens DROP COLUMN id;
