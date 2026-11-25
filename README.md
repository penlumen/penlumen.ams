# Penlumen Assess (AMS)

Tests, continuous assessment (CA) and exams for schools that use
[Penlumen School](https://github.com/penlumen/school). Students sit
computer-based tests; objective questions are marked automatically, teachers
mark theory answers, and the scores go straight onto each student's report in
Penlumen School.

Penlumen School is the hub: people sign in through it (OpenID Connect), the
app reads students, classes, subjects and terms from it, and writes scores
back to it. Its `docs/integrations.md` is the contract.

## Status

- **Assessments:** two types, matching the report card: an assessment (the
  CA column) or an examination. Teachers pick a class, then one of that class's subjects
  (both from Penlumen School), set the timing and report marks, and add
  objective and theory (essay) questions. Questions can be edited or removed
  while it's a draft; details can be edited until students start; an exam
  can be scheduled, ended early, or reopened with a new closing time, or
  deleted for good.
- **Formatted text and pictures:** questions and essay answers have a small
  editor: bold, italic, underline, superscript and subscript (x², H₂O),
  lists, and pictures (from a file, or pasted). Students can add a photo of
  a diagram or their working. Pictures only open for staff of that exam and
  the student who added them, and the text is cleaned of anything else
  before it's saved.
- **CSV:** questions can be imported from a spreadsheet and exported (a
  template is provided) as plain text; submissions export as CSV too.
- **Taking it:** students answer one question at a time; answers (essays
  too) save as they go and wait on the device if the connection drops. The
  timer is kept by the server.
- **Safe Exam Browser (optional):** an exam can require
  [SEB](https://safeexambrowser.org), the locked-down exam browser. AMS
  gives each such exam an SEB settings file and an "Open in Safe Exam
  Browser" button, and refuses requests from any other browser. Paste the
  exam's Config Key from the SEB Config Tool for a secure check of every
  request; without it, AMS only checks that the browser says it's SEB.
- **Camera, sound and screen monitoring (optional):** students share their
  camera, microphone and entire screen to start. Leaving the page, talking
  or noise, or a device being turned off raises an alert and takes a photo
  of the student and their screen (plus one photo at the start); sound is
  never recorded. Photos and alerts are deleted 30 days after the exam
  closes.
- **Monitoring:** every student in the class, who is writing, offline, has
  left the page or not started, refreshing every few seconds; extra time
  for one student or everyone.
- **Marking:** objective answers mark themselves; essays are marked by
  script number (names hidden) against the marking guide.
- **Scores and reports:** once everything is marked, scores go to each
  report card in Penlumen School, scaled to the report (24/30 on a test worth
  10 CA marks becomes 8). Reports show averages, pass rate, the spread of
  scores and how each question went; results export as CSV.
- **Looks like the school portal:** it uses the school's theme colour from
  Penlumen School.

## For a school: double-click ams.exe

Put `ams.exe` in its own folder (e.g. `C:\Exams`) and double-click it. A
window opens saying Exams is running, and the browser opens at
http://localhost:8080. Close the window to stop Exams.

On first run it creates its settings file (`.env`, with a random
`SESSION_SECRET`) and its database (`penlumen_ams.db`) next to itself. The
setup page then asks how the school uses Exams: **on its own**, or **with
Penlumen School** (its API address, plus the client ID and secret from
School Settings › Integrations, with a **Test connection** button). Later,
administrators change the port, address, time zone and connection under
**Settings**; saving rewrites `.env` and restarts Exams in place.

For safety, Penlumen School's secrets can only be entered on the setup page
from the computer running Exams; after that, only administrators can change
them. Settings given as environment variables win over `.env` and show as
read-only.

Windows may warn about an unrecognised app the first time (choose **More
info › Run anyway**) and ask to allow network access (allow it on private
networks, so students' computers can reach Exams).

## Running it locally

You need [Go](https://go.dev/dl/) 1.26 or newer.

```bash
go run ./cmd/ams          # makes .env with a SESSION_SECRET if there isn't one
```

Open http://localhost:8080. Settings are read from the environment, then
`.env` in the working directory, then `.env` next to the program.

### With or without Penlumen School

AMS keeps its own copy of the school: classes, terms, students and staff.

- **On its own** (no `SMS_CLIENT_ID`): the first visit asks for the school's
  name and the first administrator. Administrators then add classes, terms,
  staff and students on the **People** page; staff sign in with their email
  and a password, students with their reg number and a PIN.
- **With Penlumen School** (its API on port 5445 locally): AMS syncs its
  classes, terms, students and staff on start, every 10 minutes, and at once
  when its webhooks say something changed. Penlumen School is the master for
  its people: their details change there, and someone removed there can't
  sign in. Administrators can still add people of AMS's own, and set
  anyone's AMS password or PIN.

Either way everyone can sign in with an AMS password or PIN, so exams go on
when Penlumen School is down. "Sign in with your school portal" is offered
too when it's connected. Sending scores to report cards needs Penlumen
School; on its own, export them as CSV.

### The database

With no `DATABASE_URL`, everything is kept in one SQLite file, `penlumen_ams.db`, in
the folder the app runs from: nothing to install, and backing up is copying
that file (with the app stopped). That suits one school on one computer or
server.

For several app servers sharing one database, or a managed database, use
Postgres instead:

```bash
docker compose up -d db   # Postgres on port 5433
DATABASE_URL=postgres://ams:ams@localhost:5433/penlumen_ams?sslmode=disable go run ./cmd/ams
```

`DATABASE_URL` takes `sqlite:path/to/file.db` (or just the path) or a
`postgres://` URL. The schema is created and kept up to date when the app
starts, either way. Moving data from one to the other isn't built in.

To get the client ID and secret, open Penlumen School as an administrator, go
to **School Settings › Integrations › Exams**, add
`http://localhost:8080/auth/callback` as a redirect URI, create a client secret
and switch the app on.

## Tests

```bash
go test ./...
```

The end-to-end tests in `internal/web` run the whole app against a fake
Penlumen School, each on a fresh SQLite file. To run them on Postgres too,
point them at a Postgres database they may wipe (the Docker one, say, if it holds nothing you need):

```bash
AMS_TEST_DATABASE_URL=postgres://ams:ams@localhost:5433/penlumen_ams?sslmode=disable go test ./...
```

## How it's built

| Folder | What's in it |
|---|---|
| `cmd/ams` | The program's entry point |
| `internal/config` | Settings from environment variables or `.env` |
| `internal/db` | Opening SQLite or Postgres, and migrations (`migrations/sqlite`, `migrations/postgres`) |
| `internal/auth` | Signing in through Penlumen School, session cookies |
| `internal/schoolsync` | Copying the school from Penlumen School into AMS |
| `internal/directory` | What pages read about the school (AMS's own copy) |
| `internal/sms` | Client for Penlumen School's data API and scores |
| `internal/store` | Every database query, as plain SQL both databases understand |
| `internal/seb` | Checking Safe Exam Browser requests, and its settings file |
| `internal/marking` | Marking answers and scaling to the report (a good first read) |
| `internal/web` | Routes, pages (`templates/`), styles and the exam screen script (`static/`) |

The stack is kept small on purpose: Go's standard library for HTTP and
templates, `modernc.org/sqlite` (pure Go, so the binary still builds without a
C compiler) and `pgx` for the database, `goose` for migrations, and `go-oidc` and
`oauth2` for signing in. Everything (templates, styles, migrations) is built
into one binary.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
