package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSchool stands in for Penlumen School: a token endpoint plus the routes
// a test registers.
func fakeSchool(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/oidc/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "pl_ems_test" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
	})
	for pattern, h := range routes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/api/v1/oidc", "pl_ems_test", "s3cret")
}

func TestRecordScores(t *testing.T) {
	var got Scores
	c := fakeSchool(t, map[string]http.HandlerFunc{
		"POST /api/v1/integrations/v1/scores": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(w, `{"data":{"saved":1,"skipped":1,"results":[
				{"student_uuid":"s1","status":"saved","assessment":22,"examination":48},
				{"student_uuid":"s2","status":"skipped","message":"This report is already approved."}]}}`)
		},
	})

	res, err := c.RecordScores(context.Background(), Scores{
		SourceID:  "exam-1",
		Subject:   "Mathematics",
		Component: "examination",
		Scores:    []StudentMark{{"s1", 48}, {"s2", 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceID != "exam-1" || got.Component != "examination" || len(got.Scores) != 2 {
		t.Errorf("sent %+v", got)
	}
	if res.Saved != 1 || res.Skipped != 1 || *res.Results[0].Examination != 48 {
		t.Errorf("got %+v", res)
	}
	if !strings.Contains(res.Results[1].Message, "approved") {
		t.Errorf("skip message = %q", res.Results[1].Message)
	}
}

func TestClassesReadsEveryPage(t *testing.T) {
	c := fakeSchool(t, map[string]http.HandlerFunc{
		"GET /api/v1/integrations/v1/classes": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "1":
				fmt.Fprint(w, `{"data":[{"uuid":"c1","name":"JSS 1","subjects":["Mathematics"]}],"total":2}`)
			case "2":
				fmt.Fprint(w, `{"data":[{"uuid":"c2","name":"JSS 2","subjects":[]}],"total":2}`)
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		},
	})

	classes, err := c.Classes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(classes) != 2 || classes[1].Name != "JSS 2" || classes[0].Subjects[0] != "Mathematics" {
		t.Errorf("got %+v", classes)
	}
}

func TestErrorsCarryTheSchoolsMessage(t *testing.T) {
	c := fakeSchool(t, map[string]http.HandlerFunc{
		"POST /api/v1/integrations/v1/scores": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"status":422,"success":false,"message":"component must be assessment or examination"}`)
		},
	})

	_, err := c.RecordScores(context.Background(), Scores{SourceID: "x", Subject: "Maths", Component: "quiz"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 || !strings.Contains(apiErr.Message, "component") {
		t.Fatalf("err = %v", err)
	}
}
