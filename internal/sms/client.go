// Package sms talks to Penlumen School (the student management system): it
// reads students, classes and terms, and records scores on students' reports.
// See docs/integrations.md in the school repository for the contract.
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"
)

// Scopes the app asks for with its client credentials token.
const (
	ScopeRead   = "school.read"
	ScopeScores = "results.write"
)

type Client struct {
	base string
	http *http.Client
}

// New makes a client for the school whose issuer and credentials are given.
// Tokens are fetched and refreshed automatically.
func New(issuer, clientID, clientSecret string) *Client {
	issuer = strings.TrimRight(issuer, "/")
	creds := clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     issuer + "/token",
		Scopes:       []string{ScopeRead, ScopeScores},
	}
	httpClient := creds.Client(context.Background())
	httpClient.Timeout = 20 * time.Second
	return &Client{
		// https://host/api/v1/oidc -> https://host/api/v1/integrations/v1
		base: strings.TrimSuffix(issuer, "/oidc") + "/integrations/v1",
		http: httpClient,
	}
}

// APIError is a non-2xx answer from Penlumen School.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("penlumen school: %d %s", e.Status, e.Message)
}

type Branch struct {
	UUID           string  `json:"uuid"`
	Name           string  `json:"name"`
	AssessmentMax  float64 `json:"assessment_max"`
	ExaminationMax float64 `json:"examination_max"`
}

type School struct {
	UUID     string   `json:"uuid"`
	Name     string   `json:"name"`
	Slug     string   `json:"slug"`
	Theme    *Theme   `json:"theme"`
	Branches []Branch `json:"branches"`
}

// Theme is the brand colour set in School Settings › Theme.
type Theme struct {
	Preset  *string `json:"preset"`
	Primary string  `json:"primary"`
}

type Term struct {
	UUID       string     `json:"uuid"`
	BranchUUID string     `json:"branch_uuid"`
	Session    string     `json:"session"`
	Term       string     `json:"term"`
	OpenDate   *time.Time `json:"open_date"`
	CloseDate  *time.Time `json:"close_date"`
	// ACTIVE marks the branch's current term.
	Status string `json:"status"`
}

type Class struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	BranchUUID   string   `json:"branch_uuid"`
	TeacherUUID  *string  `json:"teacher_uuid"`
	TeachersUUID []string `json:"teachers_uuid"`
	Subjects     []string `json:"subjects"`
	// SubjectTeachers is each subject with the teacher who teaches it in
	// this class.
	SubjectTeachers []SubjectTeacher `json:"subject_teachers"`
}

type SubjectTeacher struct {
	UUID        string  `json:"uuid"`
	Name        string  `json:"name"`
	TeacherUUID *string `json:"teacher_uuid"`
}

type Student struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	RegNumber  string `json:"reg_number"`
	Status     string `json:"status"`
	BranchUUID string `json:"branch_uuid"`
	Class      struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"class"`
}

func (c *Client) School(ctx context.Context) (School, error) {
	var out struct{ Data School }
	err := c.do(ctx, http.MethodGet, "/school", nil, &out)
	return out.Data, err
}

// Terms of every branch, or of one branch when branchUUID isn't empty.
func (c *Client) Terms(ctx context.Context, branchUUID string) ([]Term, error) {
	q := url.Values{}
	if branchUUID != "" {
		q.Set("branch_uuid", branchUUID)
	}
	var out struct{ Data []Term }
	err := c.do(ctx, http.MethodGet, "/terms?"+q.Encode(), nil, &out)
	return out.Data, err
}

func (c *Client) Classes(ctx context.Context) ([]Class, error) {
	return pages[Class](ctx, c, "/classes", url.Values{})
}

// Students in one class (active ones only).
func (c *Client) Students(ctx context.Context, classUUID string) ([]Student, error) {
	q := url.Values{"class_uuid": {classUUID}, "status": {"active"}}
	return pages[Student](ctx, c, "/students", q)
}

// AllStudents is every student, active or not (for the sync).
func (c *Client) AllStudents(ctx context.Context) ([]Student, error) {
	return pages[Student](ctx, c, "/students", url.Values{})
}

// StaffMember is a teacher or administrator.
type StaffMember struct {
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Role        string   `json:"role"`     // staff or admin
	Position    string   `json:"position"` // ACADEMIC or ADMINISTRATIVE
	BranchUUIDs []string `json:"branch_uuids"`
}

// Staff is every teacher and administrator in the app's branches.
func (c *Client) Staff(ctx context.Context) ([]StaffMember, error) {
	return pages[StaffMember](ctx, c, "/staff", url.Values{})
}

// Scores sends one test's scores. SourceID is this app's id for the test, so
// sending it again updates the scores instead of adding to them.
type Scores struct {
	SourceID string `json:"source_id"`
	Title    string `json:"title,omitempty"`
	Subject  string `json:"subject"`
	// "assessment" or "examination".
	Component string `json:"component"`
	// Empty means the branch's active term.
	CalendarUUID string        `json:"calendar_uuid,omitempty"`
	Scores       []StudentMark `json:"scores"`
}

// StudentMark is marks towards the report's component, already on the
// report's scale (out of the branch's assessment_max or examination_max).
type StudentMark struct {
	StudentUUID string  `json:"student_uuid"`
	Score       float64 `json:"score"`
}

type ScoreOutcome struct {
	StudentUUID string `json:"student_uuid"`
	// "saved" or "skipped"; Message says why a score was skipped.
	Status      string   `json:"status"`
	Message     string   `json:"message,omitempty"`
	Assessment  *float64 `json:"assessment,omitempty"`
	Examination *float64 `json:"examination,omitempty"`
}

type ScoresResult struct {
	Saved   int            `json:"saved"`
	Skipped int            `json:"skipped"`
	Results []ScoreOutcome `json:"results"`
}

// RecordScores puts scores on students' reports. Students whose report is
// already approved, or whose total would go over the branch's limit, are
// skipped and listed in the result; the rest are saved.
func (c *Client) RecordScores(ctx context.Context, s Scores) (ScoresResult, error) {
	var out struct{ Data ScoresResult }
	err := c.do(ctx, http.MethodPost, "/scores", s, &out)
	return out.Data, err
}

// WithdrawScores takes a test's scores off students' reports.
func (c *Client) WithdrawScores(ctx context.Context, sourceID string) error {
	return c.do(ctx, http.MethodDelete, "/scores/"+url.PathEscape(sourceID), nil, nil)
}

// WithdrawStudentScore takes one student's score for a test off their
// report card.
func (c *Client) WithdrawStudentScore(ctx context.Context, sourceID, studentUUID string) error {
	q := url.Values{"student_uuid": {studentUUID}}
	return c.do(ctx, http.MethodDelete, "/scores/"+url.PathEscape(sourceID)+"?"+q.Encode(), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("penlumen school: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&e)
		if e.Message == "" {
			e.Message = res.Status
		}
		return &APIError{Status: res.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// pages fetches every page of a paged list.
func pages[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	var all []T
	q.Set("per_page", "200")
	for page := 1; ; page++ {
		q.Set("page", fmt.Sprint(page))
		var out struct {
			Data  []T `json:"data"`
			Total int `json:"total"`
		}
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if len(out.Data) == 0 || len(all) >= out.Total {
			return all, nil
		}
	}
}
