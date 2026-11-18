package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter/bound"
)

// server is a recorded GitHub: canned answers for the routes the adapter
// uses, and a log of every request. Every fixture is synthetic.
type server struct {
	t  *testing.T
	mu sync.Mutex

	pulls  map[int]map[string]any
	refs   map[string]string // branch -> sha
	checks map[string][]map[string]any
	runs   []map[string]any

	// faults: path prefix -> statuses to answer, in order, before the real
	// answer.
	faults map[string][]int
	// empty answers 200 with no body for paths with this prefix, once.
	empty map[string]bool

	log    []string
	posted []map[string]any
	nextPR int
}

func newServer(t *testing.T) (*server, *Client) {
	s := &server{t: t, pulls: map[int]map[string]any{}, refs: map[string]string{}, checks: map[string][]map[string]any{},
		faults: map[string][]int{}, empty: map[string]bool{}, nextPR: 500}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	c, err := New(Config{API: ts.URL, Owner: "acme", Repo: "widgets", Trunk: "main", BranchPrefix: "shunter/",
		Token:  func() (string, error) { return "test-token", nil },
		Checks: []string{"check"}, LandedLabel: "landed", Authors: []string{"alice", "bob"},
		Policy: bound.Policy{Attempts: 3, Backoff: time.Millisecond, MaxBackoff: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func (s *server) pull(n int, st, head, headRef, baseRef, author string, labels ...string) {
	ls := []any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	s.pulls[n] = map[string]any{"number": n, "state": st, "merged": false, "merged_at": nil,
		"head": map[string]any{"ref": headRef, "sha": head}, "base": map[string]any{"ref": baseRef, "sha": "b"},
		"user": map[string]any{"login": author}, "labels": ls, "body": ""}
}

func (s *server) check(sha, name, status, conclusion string, started time.Time) {
	var c any
	if conclusion != "" {
		c = conclusion
	}
	s.checks[sha] = append(s.checks[sha], map[string]any{"name": name, "status": status, "conclusion": c,
		"started_at": started.UTC().Format(time.RFC3339), "head_sha": sha, "html_url": "https://forge.example/run/" + name,
		"output": map[string]any{"title": name + " " + conclusion, "summary": ""}})
}

func (s *server) count(method, prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.log {
		if strings.HasPrefix(l, method+" "+prefix) {
			n++
		}
	}
	return n
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets")
	s.log = append(s.log, r.Method+" "+path)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	for p, st := range s.faults {
		if strings.HasPrefix(path, p) && len(st) > 0 {
			s.faults[p] = st[1:]
			w.WriteHeader(st[0])
			fmt.Fprintf(w, `{"message":"fault %d"}`, st[0])
			return
		}
	}
	for p := range s.empty {
		if strings.HasPrefix(path, p) {
			delete(s.empty, p)
			w.WriteHeader(200)
			return
		}
	}
	var in map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			json.Unmarshal(b, &in)
			s.posted = append(s.posted, map[string]any{"method": r.Method, "path": path, "body": in})
		}
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	q := r.URL.Query()
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "pulls":
		n, _ := strconv.Atoi(parts[1])
		if p, ok := s.pulls[n]; ok {
			reply(p)
			return
		}
		w.WriteHeader(404)
		reply(map[string]string{"message": "Not Found"})
	case r.Method == "GET" && path == "/pulls":
		head := strings.TrimPrefix(q.Get("head"), "acme:")
		base := q.Get("base")
		out := []any{}
		for n := 0; n < 10000; n++ {
			p, ok := s.pulls[n]
			if !ok || (head != "" && p["head"].(map[string]any)["ref"] != head) || (base != "" && p["base"].(map[string]any)["ref"] != base) {
				continue
			}
			if st := q.Get("state"); st != "all" && p["state"] != st {
				continue
			}
			out = append(out, p)
		}
		reply(out)
	case r.Method == "POST" && path == "/pulls":
		head := in["head"].(string)
		for _, p := range s.pulls {
			if p["head"].(map[string]any)["ref"] == head && p["state"] == "open" {
				w.WriteHeader(422)
				reply(map[string]string{"message": "A pull request already exists"})
				return
			}
		}
		s.nextPR++
		s.pull(s.nextPR, "open", s.refs[head], head, in["base"].(string), "shunter-bot")
		s.pulls[s.nextPR]["body"] = in["body"]
		reply(s.pulls[s.nextPR])
	case r.Method == "PATCH" && len(parts) == 2 && parts[0] == "pulls":
		n, _ := strconv.Atoi(parts[1])
		if st, ok := in["state"].(string); ok {
			s.pulls[n]["state"] = st
		}
		if b, ok := in["base"].(string); ok {
			s.pulls[n]["base"] = map[string]any{"ref": b, "sha": "b"}
		}
		reply(s.pulls[n])
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "issues" && parts[2] == "labels":
		n, _ := strconv.Atoi(parts[1])
		for _, l := range in["labels"].([]any) {
			s.pulls[n]["labels"] = append(s.pulls[n]["labels"].([]any), map[string]any{"name": l})
		}
		reply([]any{})
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "issues" && parts[2] == "comments":
		reply(map[string]any{"id": 1})
	case r.Method == "GET" && strings.HasPrefix(path, "/git/ref/heads/"):
		b := strings.TrimPrefix(path, "/git/ref/heads/")
		sha, ok := s.refs[b]
		if !ok {
			w.WriteHeader(404)
			reply(map[string]string{"message": "Not Found"})
			return
		}
		reply(map[string]any{"ref": "refs/heads/" + b, "object": map[string]any{"sha": sha}})
	case r.Method == "DELETE" && strings.HasPrefix(path, "/git/refs/heads/"):
		b := strings.TrimPrefix(path, "/git/refs/heads/")
		if _, ok := s.refs[b]; !ok {
			w.WriteHeader(422)
			reply(map[string]string{"message": "Reference does not exist"})
			return
		}
		delete(s.refs, b)
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasPrefix(path, "/git/matching-refs/heads/"):
		pre := strings.TrimPrefix(path, "/git/matching-refs/heads/")
		out := []any{}
		for b, sha := range s.refs {
			if strings.HasPrefix(b, pre) {
				out = append(out, map[string]any{"ref": "refs/heads/" + b, "object": map[string]any{"sha": sha}})
			}
		}
		reply(out)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "commits" && parts[2] == "check-runs":
		cs := s.checks[parts[1]]
		if cs == nil {
			cs = []map[string]any{}
		}
		reply(map[string]any{"total_count": len(cs), "check_runs": cs})
	case r.Method == "GET" && (path == "/actions/runs" || (len(parts) == 4 && parts[0] == "actions" && parts[1] == "workflows" && parts[3] == "runs")):
		out := []any{}
		for _, run := range s.runs {
			if sha := q.Get("head_sha"); sha != "" && run["head_sha"] != sha {
				continue
			}
			if b := q.Get("branch"); b != "" && run["head_branch"] != b {
				continue
			}
			out = append(out, run)
		}
		reply(map[string]any{"total_count": len(out), "workflow_runs": out})
	case r.Method == "POST" && len(parts) == 4 && parts[0] == "actions" && parts[1] == "runs":
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		for _, run := range s.runs {
			if run["id"] == id {
				if run["status"] == "completed" && parts[3] == "cancel" {
					w.WriteHeader(409)
					reply(map[string]string{"message": "Cannot cancel a workflow run that is completed."})
					return
				}
				run["status"] = map[string]string{"cancel": "completed", "rerun-failed-jobs": "queued"}[parts[3]]
			}
		}
		w.WriteHeader(202)
		reply(map[string]any{})
	case r.Method == "POST" && len(parts) == 4 && parts[0] == "actions" && parts[1] == "workflows" && parts[3] == "dispatches":
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
		reply(map[string]string{"message": "no such route in the test server: " + r.Method + " " + path})
	}
}

func (s *server) run(id int64, sha, branch, status, conclusion string) {
	var c any
	if conclusion != "" {
		c = conclusion
	}
	s.runs = append(s.runs, map[string]any{"id": id, "head_sha": sha, "head_branch": branch, "status": status, "conclusion": c, "name": "ci"})
}
