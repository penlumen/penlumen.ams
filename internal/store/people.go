package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// AMS's own copy of the school: its staff (the users table), students (the
// students table), classes and terms. Rows from Penlumen School (Source
// "sms") are kept in step by package schoolsync, and Penlumen School wins;
// rows added in AMS (Source "local") are its own. Either way, everyone can
// sign in to AMS with a password (staff) or PIN (students) set here, so AMS
// works when Penlumen School is down.
//
// Person is either one: pages treat staff and students alike where they can
// (sign-in, the People page), and the store picks the table by Role.

const (
	SourceSMS   = "sms"
	SourceLocal = "local"
)

// ErrTaken means someone else already signs in with that email or reg
// number.
var ErrTaken = errors.New("already in use")

type Person struct {
	UUID        string
	Role        string // admin or staff (users), or student (students)
	Source      string // sms or local
	Name        string
	Email       string   // staff
	RegNumber   string   // students
	ClassUUID   string   // students
	BranchUUIDs []string // staff
	// Position of staff: academic (teachers: assessments only for the
	// subjects they teach) or administrative (every subject).
	Position    string
	Status      string // active or inactive
	HasPassword bool   // a password (staff) or PIN (students) is set
}

func (p Person) IsStudent() bool { return p.Role == "student" }
func (p Person) Active() bool    { return p.Status == "active" }

// AllSubjects is true for administrators and administrative staff: they can
// set assessments for any subject, not only ones they teach.
func (p Person) AllSubjects() bool { return p.Role == "admin" || p.Position == "administrative" }

func positionOr(p string) string {
	if p == "administrative" {
		return p
	}
	return "academic"
}

// SignInName is how the person signs in: email for staff, reg number for
// students. Both are kept in lower case.
func SignInName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// The two tables as one, for reads that don't mind which (secret is the
// password or PIN hash).
const everyone = `(
	SELECT uuid, role, source, name, email, NULL AS reg_number, NULL AS class_uuid,
	       branch_uuids, position, status, password_hash AS secret FROM users
	UNION ALL
	SELECT uuid, 'student', source, name, NULL, reg_number, class_uuid,
	       '[]', '', status, pin_hash FROM students) p`

const personColumns = `p.uuid, p.role, p.source, p.name, coalesce(p.email, ''), coalesce(p.reg_number, ''),
	coalesce(p.class_uuid, ''), p.branch_uuids, p.position, p.status, p.secret IS NOT NULL`

func personFields(p *Person) []any {
	return []any{&p.UUID, &p.Role, &p.Source, &p.Name, &p.Email, &p.RegNumber,
		&p.ClassUUID, (*jsonList)(&p.BranchUUIDs), &p.Position, &p.Status, &p.HasPassword}
}

func scanPerson(row scanner) (Person, error) {
	var p Person
	err := row.Scan(personFields(&p)...)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (s *Store) collectPeople(rows *sql.Rows, err error) ([]Person, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// table is where a role's people are kept, and the column of their secret.
func table(role string) (name, secret string) {
	if role == "student" {
		return "students", "pin_hash"
	}
	return "users", "password_hash"
}

func (s *Store) Person(ctx context.Context, uuid string) (Person, error) {
	return scanPerson(s.c().row(ctx, `SELECT `+personColumns+` FROM `+everyone+` WHERE p.uuid = $1`, uuid))
}

// PersonFor finds who signs in with an email (staff) or reg number
// (students), with their password or PIN hash.
func (s *Store) PersonFor(ctx context.Context, student bool, signIn string) (Person, string, error) {
	q := `SELECT ` + personColumns + `, coalesce(p.secret, '') FROM ` + everyone + ` WHERE p.role <> 'student' AND p.email = $1`
	if student {
		q = `SELECT ` + personColumns + `, coalesce(p.secret, '') FROM ` + everyone + ` WHERE p.role = 'student' AND p.reg_number = $1`
	}
	var p Person
	var hash string
	err := s.c().row(ctx, q, SignInName(signIn)).Scan(append(personFields(&p), &hash)...)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", ErrNotFound
	}
	return p, hash, err
}

// Students lists a class's active students, by name.
func (s *Store) Students(ctx context.Context, classUUID string) ([]Person, error) {
	return s.collectPeople(s.c().query(ctx, `SELECT `+personColumns+` FROM `+everyone+`
		WHERE p.role = 'student' AND p.status = 'active' AND p.class_uuid = $1 ORDER BY p.name`, classUUID))
}

// People lists the students (optionally of one class) or the staff, with
// inactive ones last.
func (s *Store) People(ctx context.Context, students bool, classUUID string) ([]Person, error) {
	q := `SELECT ` + personColumns + ` FROM ` + everyone + ` WHERE `
	args := []any{}
	if students {
		q += `p.role = 'student'`
		if classUUID != "" {
			q += ` AND p.class_uuid = $1`
			args = append(args, classUUID)
		}
	} else {
		q += `p.role <> 'student'`
	}
	q += ` ORDER BY p.status, p.name`
	return s.collectPeople(s.c().query(ctx, q, args...))
}

// HasAdminLogin reports whether any active administrator can sign in to
// AMS with a password (if not, a new install asks for one).
func (s *Store) HasAdminLogin(ctx context.Context) (bool, error) {
	var ok bool
	err := s.c().row(ctx, `SELECT EXISTS (SELECT 1 FROM users
		WHERE role = 'admin' AND status = 'active' AND password_hash IS NOT NULL)`).Scan(&ok)
	return ok, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrTaken
	}
	return err
}

// AddPerson adds someone in AMS (Source local) and returns their uuid.
func (s *Store) AddPerson(ctx context.Context, p Person, secretHash string) (string, error) {
	p.UUID = newID()
	var err error
	if p.IsStudent() {
		_, err = s.c().exec(ctx, `
			INSERT INTO students (uuid, source, name, reg_number, class_uuid, pin_hash)
			VALUES ($1, 'local', $2, $3, $4, $5)`,
			p.UUID, strings.TrimSpace(p.Name), nullable(SignInName(p.RegNumber)), nullable(p.ClassUUID), nullable(secretHash))
	} else {
		_, err = s.c().exec(ctx, `
			INSERT INTO users (uuid, role, source, name, email, branch_uuids, position, password_hash)
			VALUES ($1, $2, 'local', $3, $4, $5, $6, $7)`,
			p.UUID, p.Role, strings.TrimSpace(p.Name), nullable(SignInName(p.Email)), jsonList(p.BranchUUIDs),
			positionOr(p.Position), nullable(secretHash))
	}
	return p.UUID, uniqueErr(err)
}

// UpdatePerson saves changes to someone added in AMS. People from Penlumen
// School are changed there. A student stays a student, and staff stay staff.
func (s *Store) UpdatePerson(ctx context.Context, p Person) error {
	var n int64
	var err error
	if p.IsStudent() {
		n, err = s.c().exec(ctx, `
			UPDATE students SET name = $2, reg_number = $3, class_uuid = $4, updated_at = now()
			WHERE uuid = $1 AND source = 'local'`,
			p.UUID, strings.TrimSpace(p.Name), nullable(SignInName(p.RegNumber)), nullable(p.ClassUUID))
	} else {
		n, err = s.c().exec(ctx, `
			UPDATE users SET role = $2, name = $3, email = $4, branch_uuids = $5, position = $6, updated_at = now()
			WHERE uuid = $1 AND source = 'local'`,
			p.UUID, p.Role, strings.TrimSpace(p.Name), nullable(SignInName(p.Email)), jsonList(p.BranchUUIDs),
			positionOr(p.Position))
	}
	if err != nil {
		return uniqueErr(err)
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// RemovePerson deletes someone added in AMS. A student's exam attempts stay
// (with their name), so marks and reports aren't lost.
func (s *Store) RemovePerson(ctx context.Context, uuid string) error {
	var removed int64
	for _, t := range []string{"users", "students"} {
		n, err := s.c().exec(ctx, `DELETE FROM `+t+` WHERE uuid = $1 AND source = 'local'`, uuid)
		if err != nil {
			return err
		}
		removed += n
	}
	if removed == 0 {
		return ErrStatus
	}
	return nil
}

// SetPassword sets (or with "" clears) someone's AMS password or PIN hash.
func (s *Store) SetPassword(ctx context.Context, uuid, hash string) error {
	var changed int64
	for _, t := range []string{"users", "students"} {
		_, col := table(map[string]string{"users": "staff", "students": "student"}[t])
		n, err := s.c().exec(ctx, `UPDATE `+t+` SET `+col+` = $2, updated_at = now() WHERE uuid = $1`, uuid, nullable(hash))
		if err != nil {
			return err
		}
		changed += n
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- From Penlumen School ----------

// SyncPerson saves someone from Penlumen School, keeping their AMS password
// or PIN. Someone added in AMS with the same email or reg number (while
// Penlumen School was down, say) becomes this person: their attempts move to
// Penlumen School's uuid and their password or PIN is kept.
func (s *Store) SyncPerson(ctx context.Context, p Person) error {
	tx, c, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tbl, secret := table(p.Role)
	key, value := "email", SignInName(p.Email)
	if p.IsStudent() {
		key, value = "reg_number", SignInName(p.RegNumber)
	}

	// Someone else in the same table already signing in with it.
	keepHash := ""
	if value != "" {
		rows, err := c.query(ctx, `SELECT uuid, source, coalesce(`+secret+`, '') FROM `+tbl+`
			WHERE uuid <> $1 AND `+key+` = $2`, p.UUID, value)
		if err != nil {
			return err
		}
		type clash struct{ uuid, source, hash string }
		var clashes []clash
		for rows.Next() {
			var cl clash
			if err := rows.Scan(&cl.uuid, &cl.source, &cl.hash); err != nil {
				rows.Close()
				return err
			}
			clashes = append(clashes, cl)
		}
		rows.Close()
		for _, cl := range clashes {
			if cl.source == SourceLocal {
				// The same person, added here first: merge.
				if p.IsStudent() {
					if _, err := c.exec(ctx, `UPDATE attempts SET student_uuid = $2 WHERE student_uuid = $1`, cl.uuid, p.UUID); err != nil {
						return err
					}
				}
				if _, err := c.exec(ctx, `DELETE FROM `+tbl+` WHERE uuid = $1`, cl.uuid); err != nil {
					return err
				}
				keepHash = cl.hash
			} else if _, err := c.exec(ctx, `UPDATE `+tbl+` SET `+key+` = NULL WHERE uuid = $1`, cl.uuid); err != nil {
				// Another Penlumen School person had it (it changed there):
				// theirs is out of date until they sync.
				return err
			}
		}
	}

	if p.IsStudent() {
		_, err = c.exec(ctx, `
			INSERT INTO students (uuid, source, name, reg_number, class_uuid, status, pin_hash)
			VALUES ($1, 'sms', $2, $3, $4, $5, $6)
			ON CONFLICT (uuid) DO UPDATE SET source = 'sms', name = excluded.name,
				reg_number = excluded.reg_number, class_uuid = excluded.class_uuid, status = excluded.status,
				pin_hash = coalesce(students.pin_hash, excluded.pin_hash), updated_at = now()`,
			p.UUID, p.Name, nullable(value), nullable(p.ClassUUID), statusOr(p.Status), nullable(keepHash))
	} else {
		_, err = c.exec(ctx, `
			INSERT INTO users (uuid, role, source, name, email, branch_uuids, position, status, password_hash)
			VALUES ($1, $2, 'sms', $3, $4, $5, $6, $7, $8)
			ON CONFLICT (uuid) DO UPDATE SET role = excluded.role, source = 'sms', name = excluded.name,
				email = excluded.email, branch_uuids = excluded.branch_uuids, position = excluded.position,
				status = excluded.status,
				password_hash = coalesce(users.password_hash, excluded.password_hash), updated_at = now()`,
			p.UUID, p.Role, p.Name, nullable(value), jsonList(p.BranchUUIDs), positionOr(p.Position),
			statusOr(p.Status), nullable(keepHash))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func statusOr(s string) string {
	if strings.EqualFold(s, "inactive") {
		return "inactive"
	}
	return "active"
}

// DeactivateMissing marks Penlumen School students (kind "student") or staff
// who weren't in a full sync as inactive: they were removed there.
func (s *Store) DeactivateMissing(ctx context.Context, kind string, seen []string) (int64, error) {
	tbl, _ := table(kind)
	list, args := in(1, seen)
	q := `UPDATE ` + tbl + ` SET status = 'inactive', updated_at = now()
		WHERE source = 'sms' AND status = 'active'`
	if len(seen) > 0 {
		q += ` AND uuid NOT IN (` + list + `)`
	}
	return s.c().exec(ctx, q, args...)
}

// ---------- Classes and terms ----------

type Class struct {
	UUID       string
	Name       string
	BranchUUID string
	Subjects   []string
	// Teachers maps a subject to the uuid of the teacher who teaches it
	// in this class (subjects with no teacher yet are missing).
	Teachers map[string]string
	Source   string
}

type Term struct {
	UUID       string
	BranchUUID string
	Session    string
	Term       string
	Status     string // ACTIVE for the current one
	Source     string
}

func (s *Store) Classes(ctx context.Context) ([]Class, error) {
	rows, err := s.c().query(ctx, `SELECT uuid, name, branch_uuid, subjects, subject_teachers, source FROM classes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Class
	for rows.Next() {
		var cl Class
		if err := rows.Scan(&cl.UUID, &cl.Name, &cl.BranchUUID, (*jsonList)(&cl.Subjects), (*jsonMap)(&cl.Teachers), &cl.Source); err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

func (s *Store) Terms(ctx context.Context) ([]Term, error) {
	rows, err := s.c().query(ctx, `SELECT uuid, branch_uuid, session, term, status, source FROM terms ORDER BY session DESC, term`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Term
	for rows.Next() {
		var t Term
		if err := rows.Scan(&t.UUID, &t.BranchUUID, &t.Session, &t.Term, &t.Status, &t.Source); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveClass adds or updates a class. A local class can't take over one from
// Penlumen School.
func (s *Store) SaveClass(ctx context.Context, cl Class) (string, error) {
	if cl.UUID == "" {
		cl.UUID = newID()
	}
	n, err := s.c().exec(ctx, `
		INSERT INTO classes (uuid, name, branch_uuid, subjects, subject_teachers, source) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (uuid) DO UPDATE SET name = excluded.name, branch_uuid = excluded.branch_uuid,
			subjects = excluded.subjects, subject_teachers = excluded.subject_teachers,
			source = excluded.source, updated_at = now()
		WHERE classes.source = excluded.source OR excluded.source = 'sms'`,
		cl.UUID, strings.TrimSpace(cl.Name), cl.BranchUUID, jsonList(cl.Subjects), jsonMap(cl.Teachers), cl.Source)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrStatus
	}
	return cl.UUID, nil
}

// RemoveClass deletes a class added in AMS that has no students.
func (s *Store) RemoveClass(ctx context.Context, uuid string) error {
	var students int
	if err := s.c().row(ctx, `SELECT count(*) FROM students WHERE class_uuid = $1`, uuid).Scan(&students); err != nil {
		return err
	}
	if students > 0 {
		return ErrTaken
	}
	n, err := s.c().exec(ctx, `DELETE FROM classes WHERE uuid = $1 AND source = 'local'`, uuid)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// SaveTerm adds or updates a term; an ACTIVE one makes the branch's other
// terms of the same source not current.
func (s *Store) SaveTerm(ctx context.Context, t Term) (string, error) {
	if t.UUID == "" {
		t.UUID = newID()
	}
	tx, c, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if t.Status == "ACTIVE" && t.Source == SourceLocal {
		if _, err := c.exec(ctx, `UPDATE terms SET status = '' WHERE branch_uuid = $1 AND source = 'local'`, t.BranchUUID); err != nil {
			return "", err
		}
	}
	n, err := c.exec(ctx, `
		INSERT INTO terms (uuid, branch_uuid, session, term, status, source) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (uuid) DO UPDATE SET branch_uuid = excluded.branch_uuid, session = excluded.session,
			term = excluded.term, status = excluded.status, source = excluded.source, updated_at = now()
		WHERE terms.source = excluded.source OR excluded.source = 'sms'`,
		t.UUID, t.BranchUUID, strings.TrimSpace(t.Session), strings.TrimSpace(t.Term), t.Status, t.Source)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrStatus
	}
	return t.UUID, tx.Commit()
}

// RemoveTerm deletes a term added in AMS that no assessment uses.
func (s *Store) RemoveTerm(ctx context.Context, uuid string) error {
	var used int
	if err := s.c().row(ctx, `SELECT count(*) FROM assessments WHERE calendar_uuid = $1`, uuid).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return ErrTaken
	}
	n, err := s.c().exec(ctx, `DELETE FROM terms WHERE uuid = $1 AND source = 'local'`, uuid)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStatus
	}
	return nil
}

// PruneSynced deletes Penlumen School classes and terms that a full sync
// didn't see (removed there). table is "classes" or "terms".
func (s *Store) PruneSynced(ctx context.Context, table string, seen []string) error {
	if table != "classes" && table != "terms" {
		return errors.New("prune: unknown table")
	}
	list, args := in(1, seen)
	q := `DELETE FROM ` + table + ` WHERE source = 'sms'`
	if len(seen) > 0 {
		q += ` AND uuid NOT IN (` + list + `)`
	}
	_, err := s.c().exec(ctx, q, args...)
	return err
}

// ---------- Settings ----------

// Setting reads a JSON setting into out; ErrNotFound if it isn't set.
func (s *Store) Setting(ctx context.Context, key string, out any) error {
	var v string
	err := s.c().row(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(v), out)
}

func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.c().exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

// MoveSchool changes the school assessments belong to: a school that
// started on its own and then connected to Penlumen School keeps its
// assessments.
func (s *Store) MoveSchool(ctx context.Context, from, to string) error {
	_, err := s.c().exec(ctx, `UPDATE assessments SET school_uuid = $2 WHERE school_uuid = $1`, from, to)
	return err
}

// MoveBranch moves AMS's own classes, terms and assessments from one
// branch to another (the stand-alone branch to Penlumen School's).
func (s *Store) MoveBranch(ctx context.Context, from, to string) error {
	for _, q := range []string{
		`UPDATE classes SET branch_uuid = $2 WHERE branch_uuid = $1`,
		`UPDATE terms SET branch_uuid = $2 WHERE branch_uuid = $1`,
		`UPDATE assessments SET branch_uuid = $2 WHERE branch_uuid = $1`,
	} {
		if _, err := s.c().exec(ctx, q, from, to); err != nil {
			return err
		}
	}
	return nil
}
