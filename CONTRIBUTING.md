# Contributing

Thanks for helping. New to Go? That's fine: the code sticks to the standard
library where it can, and [A Tour of Go](https://go.dev/tour/) covers
everything you need to start.

## Getting set up

Follow **Running it locally** in the [README](README.md). If you only work on
code that doesn't need sign-in (the Penlumen School client, marking, the
database), `go test ./...` is enough; the tests use fake servers, not a real
Penlumen School.

## Making a change

1. Open an issue first for anything bigger than a small fix, so we can agree
   on the approach.
2. Branch from `main`.
3. Keep the change focused, and add or update tests.
4. Before pushing:

   ```bash
   gofmt -w .
   go vet ./...
   go test ./...
   ```

5. Open a pull request saying what changed and why.

## Conventions

- **Database changes** need a migration for each database: a new file in
  `internal/db/migrations/postgres` and one in `internal/db/migrations/sqlite`,
  each numbered after the last one in its folder, with `-- +goose Up` and
  `-- +goose Down` sections. Never edit a migration that's already on `main`.
- **Queries** go in `internal/store`, in SQL both databases understand. Pass
  times in as arguments rather than doing date maths in SQL; `store/sql.go`
  lists the few places the two differ. Run the tests on Postgres too
  (`AMS_TEST_DATABASE_URL`, see the README) before opening a pull request.
- **Penlumen School owns people and classes.** Store their uuids here, never
  copies of names or contact details beyond what a page needs right now.
- **Scores sent to Penlumen School** are on the report's scale (out of the
  branch's `assessment_max` or `examination_max`), and each test has a stable
  `source_id` so resending updates rather than adds.
- **Styling** is [Tailwind CSS](https://tailwindcss.com) 4: its browser
  build, `static/tailwind.js` (4.3.3, served by Exams so it works without
  internet), turns the classes into CSS as pages load, so there's no build
  step: write utility classes in the templates. Colours are the theme names
  defined in `templates/layout.html`
  (`bg-card`, `text-muted`, `border-line`, `bg-primary`, …), never raw hex, so
  the school's theme colour applies. The few repeated pieces (`btn`, `card`, `pill`,
  `input`, `field`, `icon`) are `@utility` blocks in the same file. State the
  exam script changes is a `data-` attribute styled with `data-[…]:`
  variants; don't name one `data-on…`, which html/template treats as
  JavaScript.
- **The school's colours**: the brand colour comes from Penlumen School's
  theme (`theme.go` works out the shades the same way the portal does). Use
  `bg-primary`, `bg-accent`, `text-accent-fg`, never a fixed blue. There is
  no dark mode, like the portal's student pages.
- **School data** (classes, subjects, terms, students) is read through
  `internal/directory`, which caches it and is cleared by Penlumen School's
  webhooks (`internal/webhook`).
- **Words people read** are plain and short: "Submit exam", not "Finalize
  attempt submission".
