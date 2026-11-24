-- Who teaches each subject in a class, and whether staff are academic
-- (teachers: they set assessments only for subjects they teach) or
-- administrative (every subject). Administrators can always set any.
-- Matches migrations/sqlite/00002.

-- +goose Up
-- {"Mathematics": "<user uuid>", ...}: a subject's teacher in this class.
ALTER TABLE classes ADD COLUMN subject_teachers text NOT NULL DEFAULT '{}';
ALTER TABLE users ADD COLUMN position text NOT NULL DEFAULT 'academic'
    CHECK (position IN ('academic', 'administrative'));

-- +goose Down
ALTER TABLE users DROP COLUMN position;
ALTER TABLE classes DROP COLUMN subject_teachers;
