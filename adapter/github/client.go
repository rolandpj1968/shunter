// Package github is the forge and CI adapter for GitHub (or GitHub
// Enterprise), over its REST API.
//
// A change is a pull request, named by its number. A train is a branch the
// git adapter pushes, BranchPrefix+train, and a pull request from that branch
// to trunk (or to the branch of the train it is built on). CI is either the
// checks GitHub runs on the train's pull request, or a workflow the adapter
// dispatches on the train's branch; its verdict is read from the check runs
// on the train's exact head.
//
// Every request is bounded (bound.Policy) and spaced (MinGap). An empty or
// unparseable answer to a read is a transient failure, never an answer. In
// read-only mode (shadow running) the client refuses every request but GET
// before it leaves the process.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/bound"
	"github.com/rolandpj1968/shunter/adapter/hook"
)

// Config is the adapter's configuration.
type Config struct {
	// API is the REST root. Default https://api.github.com.
	API string
	// Owner and Repo name the repository.
	Owner, Repo string
	// Trunk is the branch trains land on.
	Trunk string
	// Token returns the API token. It is called once per client and never
	// logged.
	Token func() (string, error)
	// BranchPrefix names train branches: BranchPrefix + train id.
	BranchPrefix string

	// Authors are the producers whose changes the gate accepts. Empty
	// accepts every author. A change labelled ApprovedLabel is accepted
	// whoever wrote it.
	Authors       []string
	ApprovedLabel string
	// AuthorBranches limits an author to changes from branches whose name
	// matches a regular expression (a bot that may only offer its own
	// refresh branches, say). An author named here is approved by its
	// branch alone; every other author is as Authors says.
	AuthorBranches map[string]string
	// ApprovedFile names a file of changes approved whoever wrote them:
	// one or more change ids a line, a leading # on an id dropped; a line
	// is read up to its first word that is not an id, so "# ..." is a
	// comment. It is read at every gate, so an edit counts at once. A
	// missing file approves nothing.
	ApprovedFile string
	// LandedLabel marks a change that landed in a train and was closed, not
	// merged, by the forge. A merged change is landed whatever its labels.
	LandedLabel string

	// Checks are the check names whose success is the verdict. Empty means
	// every check run on the head.
	Checks []string
	// Workflow is the workflow file (or id) CI runs as. With Dispatch, the
	// adapter dispatches it on the train's branch after opening the pull
	// request; otherwise the pull request starts it. Reruns and cancels
	// are limited to its runs when it is set.
	Workflow string
	Dispatch bool
	// Blame classifies a failure and names the lines it charges. Optional.
	Blame hook.Hook

	// ReadOnly refuses every request that is not a GET.
	ReadOnly bool
	// MinGap is the least time between two requests. Default 0.
	MinGap time.Duration
	// Policy bounds each request.
	Policy bound.Policy
	// HTTP is the client; a default one when nil.
	HTTP *http.Client
	// Now is the clock.
	Now func() time.Time
}

// Client is a GitHub adapter: it implements adapter.Forge and adapter.CI.
type Client struct {
	cfg Config
	// branchRule is AuthorBranches, compiled.
	branchRule map[string]*regexp.Regexp

	tokenOnce sync.Once
	token     string
	tokenErr  error

	gap  sync.Mutex
	last time.Time

	// Requests counts requests by method, for tests and the shadow report.
	mu       sync.Mutex
	requests map[string]int
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.Owner == "" || cfg.Repo == "" || cfg.Trunk == "" {
		return nil, errors.New("github: owner, repo and trunk are required")
	}
	if cfg.API == "" {
		cfg.API = "https://api.github.com"
	}
	cfg.API = strings.TrimRight(cfg.API, "/")
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = "shunter/"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Policy.Timeout <= 0 {
		cfg.Policy.Timeout = 60 * time.Second
	}
	c := &Client{cfg: cfg, requests: map[string]int{}, branchRule: map[string]*regexp.Regexp{}}
	for author, rx := range cfg.AuthorBranches {
		re, err := regexp.Compile(rx)
		if err != nil {
			return nil, fmt.Errorf("github: author_branches %s: %w", author, err)
		}
		c.branchRule[author] = re
	}
	return c, nil
}

// Requests returns how many requests were sent, by method.
func (c *Client) Requests() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.requests {
		out[k] = v
	}
	return out
}

func (c *Client) repo(format string, a ...any) string {
	return fmt.Sprintf("/repos/%s/%s", url.PathEscape(c.cfg.Owner), url.PathEscape(c.cfg.Repo)) + fmt.Sprintf(format, a...)
}

func (c *Client) getToken() (string, error) {
	c.tokenOnce.Do(func() {
		if c.cfg.Token == nil {
			return
		}
		c.token, c.tokenErr = c.cfg.Token()
		c.token = strings.TrimSpace(c.token)
	})
	return c.token, c.tokenErr
}

// wait spaces requests by MinGap.
func (c *Client) wait(ctx context.Context) error {
	if c.cfg.MinGap <= 0 {
		return nil
	}
	c.gap.Lock()
	defer c.gap.Unlock()
	if d := c.cfg.MinGap - time.Since(c.last); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()
	return nil
}

// errReadOnly refuses a write in read-only mode.
var errReadOnly = errors.New("github: read-only: a request that writes was refused before it was sent")

// do sends one request, bounded and retried. out, when not nil, receives the
// JSON answer; a success with an empty or unparseable body is transient.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if method != http.MethodGet && c.cfg.ReadOnly {
		return adapter.Permanent(fmt.Errorf("%w: %s %s", errReadOnly, method, path))
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return adapter.Permanent(err)
		}
	}
	tok, err := c.getToken()
	if err != nil {
		return adapter.Permanent(fmt.Errorf("github: the token: %w", err))
	}
	return bound.Do(ctx, c.cfg.Policy, method+" "+path, func(ctx context.Context) error {
		if err := c.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.cfg.API+path, bytes.NewReader(body))
		if err != nil {
			return adapter.Permanent(err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "shunter")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		c.mu.Lock()
		c.requests[method]++
		c.mu.Unlock()
		res, err := c.cfg.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		data, rerr := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		if res.StatusCode >= 300 {
			he := &bound.HTTPError{Method: method, Path: path, Status: res.StatusCode, RetryAfter: bound.ParseRetryAfter(res.Header, c.cfg.Now())}
			var msg struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &msg) == nil {
				he.Message = msg.Message
			}
			if (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests) &&
				(res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(he.Message), "rate limit")) {
				he.RateLimited = true
			}
			return he
		}
		if rerr != nil {
			return fmt.Errorf("reading the answer: %w", rerr)
		}
		if out == nil {
			return nil
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return errors.New("an empty answer, which is not an answer")
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("an answer that is not JSON: %v", err)
		}
		return nil
	})
}

// IsNotFound reports whether err is the forge's 404.
func IsNotFound(err error) bool {
	var he *bound.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

func isStatus(err error, status int) bool {
	var he *bound.HTTPError
	return errors.As(err, &he) && he.Status == status
}
