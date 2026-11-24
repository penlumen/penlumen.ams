-- Who teaches each subject in a class, and whether staff are academic
-- (teachers: they set assessments only for subjects they teach) or
-- administrative (every subject). Matches migrations/postgres/00002.

-- +goose Up
ALTER TABLE classes ADD COLUMN subject_teachers TEXT NOT NULL DEFAULT '{}';
ALTER TABLE users ADD COLUMN position TEXT NOT NULL DEFAULT 'academic'
    CHECK (position IN ('academic', 'administrative'));

-- +goose Down
ALTER TABLE users DROP COLUMN position;
ALTER TABLE classes DROP COLUMN subject_teachers;
