-- The schema for SQLite, matching migrations/postgres/00001_init.sql; a
-- change needs a new migration in both folders.
--
-- Ids are uuids made by the app, kept as text. Times are UTC text like
-- 2026-11-23 10:47:06.295 (what strftime('%Y-%m-%d %H:%M:%f', 'now') gives),
-- so comparing them as text compares them as times. JSON is kept as text.

-- +goose Up

-- ---------- The school ----------

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE classes (
    uuid        TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    branch_uuid TEXT NOT NULL,
    subjects    TEXT NOT NULL DEFAULT '[]',
    source      TEXT NOT NULL CHECK (source IN ('sms', 'local')),
    updated_at  DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE TABLE terms (
    uuid        TEXT PRIMARY KEY,
    branch_uuid TEXT NOT NULL,
    session     TEXT NOT NULL,
    term        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL CHECK (source IN ('sms', 'local')),
    updated_at  DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

-- Staff: administrators and teachers.
CREATE TABLE users (
    uuid          TEXT PRIMARY KEY,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'staff')),
    source        TEXT NOT NULL CHECK (source IN ('sms', 'local')),
    name          TEXT NOT NULL,
    email         TEXT,
    branch_uuids  TEXT NOT NULL DEFAULT '[]',
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    password_hash TEXT,
    created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);
CREATE UNIQUE INDEX users_email_idx ON users (email) WHERE email IS NOT NULL;

CREATE TABLE students (
    uuid        TEXT PRIMARY KEY,
    source      TEXT NOT NULL CHECK (source IN ('sms', 'local')),
    name        TEXT NOT NULL,
    reg_number  TEXT,
    class_uuid  TEXT,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    pin_hash    TEXT,
    created_at  DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at  DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);
CREATE UNIQUE INDEX students_reg_number_idx ON students (reg_number) WHERE reg_number IS NOT NULL;
CREATE INDEX students_class_idx ON students (class_uuid);

-- ---------- Assessments ----------

CREATE TABLE assessments (
    id               TEXT PRIMARY KEY,
    school_uuid      TEXT NOT NULL,
    branch_uuid      TEXT NOT NULL,
    class_uuid       TEXT NOT NULL DEFAULT '',
    calendar_uuid    TEXT NOT NULL,
    subject          TEXT NOT NULL,
    title            TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('ca', 'exam')),
    counts_towards   TEXT NOT NULL CHECK (counts_towards IN ('assessment', 'examination')),
    report_marks     REAL NOT NULL CHECK (report_marks > 0),
    duration_minutes INTEGER NOT NULL CHECK (duration_minutes > 0),
    opens_at         DATETIME,
    closes_at        DATETIME,
    settings         TEXT NOT NULL DEFAULT '{}',
    status           TEXT NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft', 'scheduled', 'open', 'closed', 'published')),
    created_by       TEXT NOT NULL,
    created_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    published_at     DATETIME,
    publish_report   TEXT
);
CREATE INDEX assessments_school_term_idx ON assessments (school_uuid, calendar_uuid);
CREATE INDEX assessments_class_idx ON assessments (class_uuid);

CREATE TABLE sections (
    id            TEXT PRIMARY KEY,
    assessment_id TEXT NOT NULL REFERENCES assessments ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    title         TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('objective', 'theory'))
);
CREATE INDEX sections_assessment_idx ON sections (assessment_id);

CREATE TABLE questions (
    id            TEXT PRIMARY KEY,
    section_id    TEXT NOT NULL REFERENCES sections ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    body          TEXT NOT NULL,
    options       TEXT,
    answer        INTEGER,
    marks         REAL NOT NULL CHECK (marks > 0),
    marking_guide TEXT
);
CREATE INDEX questions_section_idx ON questions (section_id);

-- ---------- Sitting them ----------

CREATE TABLE attempts (
    id             TEXT PRIMARY KEY,
    assessment_id  TEXT NOT NULL REFERENCES assessments ON DELETE CASCADE,
    student_uuid   TEXT NOT NULL,
    student_name   TEXT NOT NULL DEFAULT '',
    reg_number     TEXT NOT NULL DEFAULT '',
    class_uuid     TEXT NOT NULL DEFAULT '',
    started_at     DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    deadline       DATETIME NOT NULL,
    submitted_at   DATETIME,
    status         TEXT NOT NULL DEFAULT 'in_progress'
                   CHECK (status IN ('in_progress', 'submitted', 'expired')),
    question_order TEXT NOT NULL DEFAULT '[]',
    score          REAL,
    last_seen_at   DATETIME,
    extra_minutes  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (assessment_id, student_uuid)
);
CREATE INDEX attempts_student_idx ON attempts (student_uuid);

CREATE TABLE responses (
    attempt_id  TEXT NOT NULL REFERENCES attempts ON DELETE CASCADE,
    question_id TEXT NOT NULL REFERENCES questions ON DELETE CASCADE,
    answer      INTEGER,
    text_answer TEXT,
    flagged     BOOLEAN NOT NULL DEFAULT FALSE,
    saved_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    score       REAL,
    marked_by   TEXT,
    comment     TEXT,
    PRIMARY KEY (attempt_id, question_id)
);

CREATE TABLE images (
    id            TEXT PRIMARY KEY,
    assessment_id TEXT NOT NULL REFERENCES assessments ON DELETE CASCADE,
    attempt_id    TEXT REFERENCES attempts ON DELETE CASCADE,
    uploaded_by   TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    data          BLOB NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);
CREATE INDEX images_assessment_idx ON images (assessment_id);
CREATE INDEX images_attempt_idx ON images (attempt_id);

-- ---------- Monitoring ----------

CREATE TABLE monitoring (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id TEXT NOT NULL REFERENCES attempts ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    at         DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);
CREATE INDEX monitoring_attempt_idx ON monitoring (attempt_id, at);

CREATE TABLE snapshots (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id TEXT NOT NULL REFERENCES attempts ON DELETE CASCADE,
    source     TEXT NOT NULL CHECK (source IN ('camera', 'screen')),
    reason     TEXT NOT NULL,
    image      BLOB NOT NULL,
    taken_at   DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);
CREATE INDEX snapshots_attempt_idx ON snapshots (attempt_id, taken_at);

-- +goose Down
DROP TABLE snapshots;
DROP TABLE monitoring;
DROP TABLE images;
DROP TABLE responses;
DROP TABLE attempts;
DROP TABLE questions;
DROP TABLE sections;
DROP TABLE assessments;
DROP TABLE students;
DROP TABLE users;
DROP TABLE terms;
DROP TABLE classes;
DROP TABLE settings;
