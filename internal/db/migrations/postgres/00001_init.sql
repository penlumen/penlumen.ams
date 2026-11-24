-- The schema for Postgres. migrations/sqlite/00001_init.sql is the same for
-- SQLite; a change needs a new migration in both folders.
--
-- AMS keeps its own copy of the school (people, classes, terms) so it works
-- when Penlumen School is down, or without it. Rows from Penlumen School
-- (source 'sms') are kept in step by package schoolsync and Penlumen School
-- wins; rows added in AMS (source 'local') are AMS's own.

-- +goose Up

-- ---------- The school ----------

-- Small app-wide values: the school (name, theme, branches) as JSON.
CREATE TABLE settings (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE TABLE classes (
    uuid        text PRIMARY KEY,
    name        text NOT NULL,
    branch_uuid text NOT NULL,
    -- The subjects taught in the class (JSON list of names).
    subjects    text NOT NULL DEFAULT '[]',
    source      text NOT NULL CHECK (source IN ('sms', 'local')),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE terms (
    uuid        text PRIMARY KEY,
    branch_uuid text NOT NULL,
    session     text NOT NULL,
    term        text NOT NULL,
    -- ACTIVE marks the branch's current term.
    status      text NOT NULL DEFAULT '',
    source      text NOT NULL CHECK (source IN ('sms', 'local')),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Staff: administrators and teachers. The uuid is Penlumen School's for its
-- people, so records match whichever way someone signs in.
CREATE TABLE users (
    uuid          text PRIMARY KEY,
    -- admin also manages people, classes and terms.
    role          text NOT NULL CHECK (role IN ('admin', 'staff')),
    source        text NOT NULL CHECK (source IN ('sms', 'local')),
    name          text NOT NULL,
    -- They sign in with it; kept in lower case.
    email         text,
    -- The branches whose assessments they see (JSON list; empty: all).
    branch_uuids  text NOT NULL DEFAULT '[]',
    -- inactive: removed in Penlumen School; can't sign in.
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    -- A bcrypt hash of their AMS password.
    password_hash text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_idx ON users (email) WHERE email IS NOT NULL;

-- Students. The uuid is Penlumen School's for its students, so attempts
-- match whichever way they sign in.
CREATE TABLE students (
    uuid        text PRIMARY KEY,
    source      text NOT NULL CHECK (source IN ('sms', 'local')),
    name        text NOT NULL,
    -- They sign in with it; kept in lower case.
    reg_number  text,
    class_uuid  text,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    -- A bcrypt hash of their AMS PIN.
    pin_hash    text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX students_reg_number_idx ON students (reg_number) WHERE reg_number IS NOT NULL;
CREATE INDEX students_class_idx ON students (class_uuid);

-- ---------- Assessments ----------

CREATE TABLE assessments (
    id               uuid PRIMARY KEY,
    school_uuid      text NOT NULL,
    branch_uuid      text NOT NULL,
    -- The class that sits it.
    class_uuid       text NOT NULL DEFAULT '',
    -- The term (Penlumen School calls it a calendar).
    calendar_uuid    text NOT NULL,
    subject          text NOT NULL,
    title            text NOT NULL,
    -- ca (assessment) or exam (examination): the report card's two columns.
    kind             text NOT NULL CHECK (kind IN ('ca', 'exam')),
    -- Which score on the report it adds to.
    counts_towards   text NOT NULL CHECK (counts_towards IN ('assessment', 'examination')),
    -- What full marks on this paper are worth on the report, e.g. 60 for an
    -- exam or 10 for one CA test.
    report_marks     numeric(5, 2) NOT NULL CHECK (report_marks > 0),
    duration_minutes integer NOT NULL CHECK (duration_minutes > 0),
    opens_at         timestamptz,
    closes_at        timestamptz,
    -- Shuffling, showing the score, Safe Exam Browser, monitoring.
    settings         jsonb NOT NULL DEFAULT '{}',
    status           text NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft', 'scheduled', 'open', 'closed', 'published')),
    created_by       text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    published_at     timestamptz,
    -- Penlumen School's answer, per student, the last time scores were sent.
    publish_report   jsonb
);
CREATE INDEX assessments_school_term_idx ON assessments (school_uuid, calendar_uuid);
CREATE INDEX assessments_class_idx ON assessments (class_uuid);

-- Section A · Objective and Section B · Theory.
CREATE TABLE sections (
    id            uuid PRIMARY KEY,
    assessment_id uuid NOT NULL REFERENCES assessments ON DELETE CASCADE,
    position      integer NOT NULL,
    title         text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('objective', 'theory'))
);
CREATE INDEX sections_assessment_idx ON sections (assessment_id);

CREATE TABLE questions (
    id            uuid PRIMARY KEY,
    section_id    uuid NOT NULL REFERENCES sections ON DELETE CASCADE,
    position      integer NOT NULL,
    -- Formatted text (a little HTML; see package richtext).
    body          text NOT NULL,
    -- Objective: ["Nucleus", "Cytoplasm", ...]; theory: null.
    options       jsonb,
    -- Objective: the index of the right option; theory: null.
    answer        jsonb,
    marks         numeric(5, 2) NOT NULL CHECK (marks > 0),
    marking_guide text
);
CREATE INDEX questions_section_idx ON questions (section_id);

-- ---------- Sitting them ----------

-- One student sitting one assessment. The deadline is set by the server when
-- they start, so refreshing the page never resets the timer.
CREATE TABLE attempts (
    id             uuid PRIMARY KEY,
    assessment_id  uuid NOT NULL REFERENCES assessments ON DELETE CASCADE,
    student_uuid   text NOT NULL,
    -- Who sat it, kept so lists don't depend on the students table.
    student_name   text NOT NULL DEFAULT '',
    reg_number     text NOT NULL DEFAULT '',
    class_uuid     text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    deadline       timestamptz NOT NULL,
    submitted_at   timestamptz,
    status         text NOT NULL DEFAULT 'in_progress'
                   CHECK (status IN ('in_progress', 'submitted', 'expired')),
    -- The order their questions were shown in (JSON list of ids).
    question_order text NOT NULL DEFAULT '[]',
    -- Raw marks on the paper (before scaling to the report).
    score          numeric(7, 2),
    -- The exam screen checks in every 30 seconds.
    last_seen_at   timestamptz,
    -- Extra time given from Monitoring, in minutes.
    extra_minutes  integer NOT NULL DEFAULT 0,
    UNIQUE (assessment_id, student_uuid)
);
CREATE INDEX attempts_student_idx ON attempts (student_uuid);

-- The latest answer to each question; each autosave replaces it.
CREATE TABLE responses (
    attempt_id  uuid NOT NULL REFERENCES attempts ON DELETE CASCADE,
    question_id uuid NOT NULL REFERENCES questions ON DELETE CASCADE,
    -- Objective: the chosen option.
    answer      jsonb,
    -- Theory: the written answer (formatted text).
    text_answer text,
    flagged     boolean NOT NULL DEFAULT false,
    saved_at    timestamptz NOT NULL DEFAULT now(),
    -- Filled in by automatic marking (objective) or a teacher (theory).
    score       numeric(5, 2),
    marked_by   text,
    comment     text,
    PRIMARY KEY (attempt_id, question_id)
);

-- Pictures in questions (staff) and written answers (students), linked
-- from the text as /images/{id}.
CREATE TABLE images (
    id            uuid PRIMARY KEY,
    assessment_id uuid NOT NULL REFERENCES assessments ON DELETE CASCADE,
    -- Set for a picture in a student's answer; empty for one in a question.
    attempt_id    uuid REFERENCES attempts ON DELETE CASCADE,
    uploaded_by   text NOT NULL,
    content_type  text NOT NULL,
    data          bytea NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX images_assessment_idx ON images (assessment_id);
CREATE INDEX images_attempt_idx ON images (attempt_id);

-- ---------- Monitoring ----------

-- What the exam screen reports: leaving the page and coming back, and with
-- camera, sound and screen monitoring, noise and devices turned off.
CREATE TABLE monitoring (
    id         bigserial PRIMARY KEY,
    attempt_id uuid NOT NULL REFERENCES attempts ON DELETE CASCADE,
    kind       text NOT NULL,
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX monitoring_attempt_idx ON monitoring (attempt_id, at);

-- Photos taken on the student's computer when something suspicious happens
-- (and one at the start). Kept 30 days after the exam closes (store.Purge).
CREATE TABLE snapshots (
    id         bigserial PRIMARY KEY,
    attempt_id uuid NOT NULL REFERENCES attempts ON DELETE CASCADE,
    -- camera (the student) or screen (what was on their screen).
    source     text NOT NULL CHECK (source IN ('camera', 'screen')),
    -- Why it was taken: start, left_page, noise, camera_off, screen_off…
    reason     text NOT NULL,
    image      bytea NOT NULL,
    taken_at   timestamptz NOT NULL DEFAULT now()
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
