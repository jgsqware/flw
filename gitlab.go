package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client talks to GitLab through the glab CLI, so it reuses glab's auth and host config.
type Client struct {
	Host    string // optional --hostname
	Me      string
	BaseURL string // https://gitlab.example.com
	offline bool   // read the disk cache only (see Cached)
	oldest  *stamp
}

func (c *Client) glab(args ...string) ([]byte, error) {
	if c.Host != "" && len(args) > 0 && args[0] == "api" {
		args = append(args, "--hostname", c.Host)
	}
	cache := cacheable(args)
	if c.offline {
		if !cache {
			return nil, errNoCache
		}
		b, at, err := cacheGet(args)
		if err == nil && c.oldest != nil {
			c.oldest.see(at)
		}
		return b, err
	}
	cmd := exec.Command("glab", args...)
	cmd.Env = append(os.Environ(), "GLAB_CHECK_UPDATE=false", "NO_PROMPT=1", "GLAB_NO_PROMPT=1", "NO_COLOR=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg == "" {
			msg = err.Error()
		}
		// keep the first meaningful line
		for _, l := range strings.Split(msg, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				msg = l
				break
			}
		}
		return out, errors.New(strings.TrimPrefix(msg, "glab: "))
	}
	if cache {
		cachePut(args, out)
	}
	return out, nil
}

// REST calls `glab api` with an HTTP method and string fields.
func (c *Client) REST(method, path string, fields map[string]string, out any) error {
	args := []string{"api", path, "-X", method}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-f", k+"="+fields[k])
	}
	b, err := c.glab(args...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(b)
		return nil
	}
	return json.Unmarshal(b, out)
}

// GQL runs a GraphQL query; vars are passed as string variables.
func (c *Client) GQL(query string, vars map[string]string, out any) error {
	args := []string{"api", "graphql", "-f", "query=" + query}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable args = stable cache key
	for _, k := range keys {
		args = append(args, "-f", k+"="+vars[k])
	}
	b, gerr := c.glab(args...)
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		if gerr != nil {
			return gerr
		}
		return fmt.Errorf("graphql: %w", err)
	}
	if len(resp.Errors) > 0 {
		return errors.New(resp.Errors[0].Message)
	}
	if gerr != nil && len(resp.Data) == 0 {
		return gerr
	}
	return json.Unmarshal(resp.Data, out)
}

func pp(fullPath string) string { return "projects/" + url.PathEscape(fullPath) }

// ---------- types ----------

type User struct {
	Username    string `json:"username"`
	Interaction *struct {
		ReviewState string `json:"reviewState"`
		Approved    bool   `json:"approved"`
	} `json:"mergeRequestInteraction"`
}

type Nodes[T any] struct {
	Nodes []T `json:"nodes"`
}

func usernames(n Nodes[User]) []string {
	out := make([]string, 0, len(n.Nodes))
	for _, u := range n.Nodes {
		out = append(out, "@"+u.Username)
	}
	return out
}

type Label struct {
	Title string `json:"title"`
}

type Pipeline struct {
	ID         string     `json:"id"` // gid or numeric
	IID        string     `json:"iid"`
	Status     string     `json:"status"`
	Ref        string     `json:"ref"`
	Sha        string     `json:"sha"`
	Source     string     `json:"source"`
	Duration   *int       `json:"duration"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	User       *User      `json:"user"`
	Commit     *struct {
		Title string `json:"title"`
	} `json:"commit"`
	Path    string `json:"path"`
	Project string `json:"-"`
	WebURL  string `json:"-"`
}

func gidNum(gid string) string {
	if i := strings.LastIndex(gid, "/"); i >= 0 {
		return gid[i+1:]
	}
	return gid
}

func (p Pipeline) Num() string { return gidNum(p.ID) }

type MR struct {
	IID          string     `json:"iid"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	State        string     `json:"state"`
	Draft        bool       `json:"draft"`
	WebURL       string     `json:"webUrl"`
	SourceBranch string     `json:"sourceBranch"`
	TargetBranch string     `json:"targetBranch"`
	MergedAt     *time.Time `json:"mergedAt"`
	DiffHeadSha  string     `json:"diffHeadSha"`
	Leftover     *Leftover  `json:"-"`
	TabTTY       string     `json:"-"` // terminal of its Rex tab, for Activity
	CreatedAt    time.Time
	UpdatedAt    time.Time `json:"updatedAt"`
	Project      *struct {
		FullPath string `json:"fullPath"`
	} `json:"project"`
	Author                     *User          `json:"author"`
	MergeUser                  *User          `json:"mergeUser"`
	HeadPipeline               *Pipeline      `json:"headPipeline"`
	Approved                   bool           `json:"approved"`
	ApprovalsLeft              *int           `json:"approvalsLeft"`
	ApprovalsRequired          *int           `json:"approvalsRequired"`
	ApprovedBy                 Nodes[User]    `json:"approvedBy"`
	Conflicts                  bool           `json:"conflicts"`
	UserNotesCount             int            `json:"userNotesCount"`
	DetailedMergeStatus        string         `json:"detailedMergeStatus"`
	ResolvableDiscussionsCount int            `json:"resolvableDiscussionsCount"`
	ResolvedDiscussionsCount   int            `json:"resolvedDiscussionsCount"`
	Reviewers                  Nodes[User]    `json:"reviewers"`
	Assignees                  Nodes[User]    `json:"assignees"`
	Labels                     Nodes[Label]   `json:"labels"`
	MergeWhenPipelineSucceeds  bool           `json:"mergeWhenPipelineSucceeds"`
	Squash                     bool           `json:"squash"`
	ShouldRemoveSourceBranch   *bool          `json:"shouldRemoveSourceBranch"`
	DiffStatsSummary           *DiffStats     `json:"diffStatsSummary"`
	Discussions                Nodes[Discuss] `json:"discussions"`
	Threads                    Nodes[Thread]  `json:"threads"`
	Timeline                   Nodes[Note]    `json:"timeline"`
	Commits                    Nodes[struct {
		CommittedDate time.Time `json:"committedDate"`
	}] `json:"commits"`
	Turn *Turn `json:"-"`

	Role string `json:"-"` // author / reviewer / assignee (dashboard)
}

func (m *MR) Path() string {
	if m.Project != nil {
		return m.Project.FullPath
	}
	return ""
}

type DiffStats struct {
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
	FileCount int `json:"fileCount"`
}

// Thread is a light view of a discussion: who opened it, who wrote last.
type Thread struct {
	Resolvable bool        `json:"resolvable"`
	Resolved   bool        `json:"resolved"`
	First      Nodes[Note] `json:"first"`
	Last       Nodes[Note] `json:"last"`
}

type Discuss struct {
	Resolved   bool        `json:"resolved"`
	Resolvable bool        `json:"resolvable"`
	Notes      Nodes[Note] `json:"notes"`
}

type Note struct {
	Author    *User     `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	System    bool      `json:"system"`
	Position  *struct {
		FilePath string `json:"filePath"`
		NewLine  *int   `json:"newLine"`
		OldLine  *int   `json:"oldLine"`
	} `json:"position"`
}

type Project struct {
	FullPath               string    `json:"fullPath"`
	Name                   string    `json:"name"`
	Description            string    `json:"description"`
	WebURL                 string    `json:"webUrl"`
	LastActivityAt         time.Time `json:"lastActivityAt"`
	OpenMergeRequestsCount int       `json:"openMergeRequestsCount"`
	OpenIssuesCount        int       `json:"openIssuesCount"`
	StarCount              int       `json:"starCount"`
}

type Issue struct {
	IID            string       `json:"iid"`
	Title          string       `json:"title"`
	State          string       `json:"state"`
	WebURL         string       `json:"webUrl"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	Author         *User        `json:"author"`
	Assignees      Nodes[User]  `json:"assignees"`
	Labels         Nodes[Label] `json:"labels"`
	UserNotesCount int          `json:"userNotesCount"`
	Project        string       `json:"-"`
}

type Job struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Duration     *int   `json:"duration"`
	AllowFailure bool   `json:"allowFailure"`
	Retryable    bool   `json:"retryable"`
	Cancelable   bool   `json:"cancelable"`
	Playable     bool   `json:"playable"`
	ManualJob    bool   `json:"manualJob"`
	StartedAt    *time.Time
	WebPath      string `json:"webPath"`
	Downstream   *struct {
		ID      string `json:"id"`
		IID     string `json:"iid"`
		Project struct {
			FullPath string `json:"fullPath"`
		} `json:"project"`
	} `json:"downstreamPipeline"`
	Stage   string `json:"-"`
	Project string `json:"-"`
	WebURL  string `json:"-"`
}

func (j Job) Num() string { return gidNum(j.ID) }

type FileDiff struct {
	Diff        string `json:"diff"`
	NewPath     string `json:"new_path"`
	OldPath     string `json:"old_path"`
	NewFile     bool   `json:"new_file"`
	DeletedFile bool   `json:"deleted_file"`
	RenamedFile bool   `json:"renamed_file"`
}

// ---------- queries ----------

const mrFields = `iid title draft state webUrl sourceBranch targetBranch updatedAt createdAt
project { fullPath } author { username }
headPipeline { id iid status finishedAt }
approved approvalsLeft approvedBy { nodes { username } }
conflicts userNotesCount detailedMergeStatus
resolvableDiscussionsCount resolvedDiscussionsCount
reviewers { nodes { username mergeRequestInteraction { reviewState approved } } } labels { nodes { title } }
threads: discussions(first: 50) { nodes { resolvable resolved
  first: notes(first: 1) { nodes { author { username } } }
  last: notes(last: 1) { nodes { author { username } createdAt } } } }
timeline: notes(last: 30) { nodes { author { username } createdAt system body } }
commits(first: 1) { nodes { committedDate } }`

func (c *Client) Bootstrap() error {
	var r struct {
		CurrentUser struct {
			Username string `json:"username"`
			WebURL   string `json:"webUrl"`
		} `json:"currentUser"`
	}
	if err := c.GQL(`query { currentUser { username webUrl } }`, nil, &r); err != nil {
		return err
	}
	c.Me = r.CurrentUser.Username
	if u, err := url.Parse(r.CurrentUser.WebURL); err == nil {
		c.BaseURL = u.Scheme + "://" + u.Host
	}
	return nil
}

// DetectProject resolves the project of the current git repo through glab.
func (c *Client) DetectProject() (string, error) {
	var p struct {
		PathWithNamespace string `json:"path_with_namespace"`
	}
	if err := c.REST("GET", "projects/:fullpath", nil, &p); err != nil {
		return "", err
	}
	return p.PathWithNamespace, nil
}

func (c *Client) MyMRs() ([]*MR, error) {
	return c.myLists([]string{"opened"}, mrFields, 50, "")
}

// doneFields is enough to show a merged or closed MR and find what is left of it locally.
const doneFields = `iid title draft state webUrl sourceBranch targetBranch updatedAt createdAt mergedAt diffHeadSha
project { fullPath } author { username }`

// MyDoneMRs lists my MRs merged or closed in the last days (to clean up what is left locally).
func (c *Client) MyDoneMRs(days int) ([]*MR, error) {
	since := time.Now().AddDate(0, 0, -days).UTC().Format("2006-01-02")
	return c.myLists([]string{"merged", "closed"}, doneFields, 30, `, updatedAfter: "`+since+`"`)
}

// myLists runs one query per list and state (GitLab caps the complexity of a single
// query) and merges the results.
func (c *Client) myLists(states []string, fields string, first int, extra string) ([]*MR, error) {
	lists := []struct{ field, role string }{
		{"reviewRequestedMergeRequests", "review"},
		{"authoredMergeRequests", "author"},
		{"assignedMergeRequests", "assignee"},
	}
	type job struct{ field, role, state string }
	var jobs []job
	for _, l := range lists {
		for _, st := range states {
			jobs = append(jobs, job{l.field, l.role, st})
		}
	}
	res := make([][]*MR, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			q := fmt.Sprintf(`query { currentUser { list: %s(state: %s, first: %d, sort: UPDATED_DESC%s) { nodes { %s } } } }`,
				j.field, j.state, first, extra, fields)
			var r struct {
				CurrentUser struct {
					List Nodes[*MR] `json:"list"`
				} `json:"currentUser"`
			}
			errs[i] = c.GQL(q, nil, &r)
			res[i] = r.CurrentUser.List.Nodes
		}(i, j)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var out []*MR
	for i, j := range jobs {
		for _, m := range res[i] {
			k := m.Path() + "!" + m.IID
			if seen[k] {
				continue
			}
			seen[k] = true
			m.Role = j.role
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *Client) ProjectMRs(path, state string) ([]*MR, error) {
	q := `query($p: ID!) { project(fullPath: $p) { mergeRequests(state: ` + state + `, first: 100, sort: UPDATED_DESC) { nodes { ` + mrFields + ` } } } }`
	var r struct {
		Project *struct {
			MergeRequests Nodes[*MR] `json:"mergeRequests"`
		} `json:"project"`
	}
	if err := c.GQL(q, map[string]string{"p": path}, &r); err != nil {
		return nil, err
	}
	if r.Project == nil {
		return nil, fmt.Errorf("project %s not found", path)
	}
	return r.Project.MergeRequests.Nodes, nil
}

func (c *Client) MRDetail(path, iid string) (*MR, error) {
	q := `query($p: ID!, $iid: String!) { project(fullPath: $p) { mergeRequest(iid: $iid) {
  ` + mrFields + ` description createdAt mergeUser { username } assignees { nodes { username } }
  approvalsRequired mergeWhenPipelineSucceeds squash shouldRemoveSourceBranch
  diffStatsSummary { additions deletions fileCount }
  discussions(first: 100) { nodes { resolved resolvable notes(first: 50) { nodes {
    author { username } body createdAt system position { filePath newLine oldLine } } } } }
} } }`
	var r struct {
		Project *struct {
			MergeRequest *MR `json:"mergeRequest"`
		} `json:"project"`
	}
	if err := c.GQL(q, map[string]string{"p": path, "iid": iid}, &r); err != nil {
		return nil, err
	}
	if r.Project == nil || r.Project.MergeRequest == nil {
		return nil, fmt.Errorf("MR !%s not found in %s", iid, path)
	}
	return r.Project.MergeRequest, nil
}

func (c *Client) MRDiffs(path, iid string) ([]FileDiff, error) {
	var out []FileDiff
	for page := 1; page <= 10; page++ {
		var batch []FileDiff
		err := c.REST("GET", fmt.Sprintf("%s/merge_requests/%s/diffs?per_page=100&page=%d", pp(path), iid, page), nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) Projects() ([]Project, error) {
	q := `query { projects(membership: true, first: 100, sort: "latest_activity_desc") { nodes {
  fullPath name description webUrl lastActivityAt openMergeRequestsCount openIssuesCount starCount } } }`
	var r struct {
		Projects Nodes[Project] `json:"projects"`
	}
	if err := c.GQL(q, nil, &r); err != nil {
		return nil, err
	}
	return r.Projects.Nodes, nil
}

func (c *Client) Pipelines(path, ref string) ([]Pipeline, error) {
	args := ""
	vars := map[string]string{"p": path}
	decl := "$p: ID!"
	if ref != "" {
		args = ", ref: $ref"
		decl += ", $ref: String"
		vars["ref"] = ref
	}
	q := `query(` + decl + `) { project(fullPath: $p) { pipelines(first: 50` + args + `) { nodes {
  id iid status ref sha source duration createdAt finishedAt path user { username } commit { title } } } } }`
	var r struct {
		Project *struct {
			Pipelines Nodes[Pipeline] `json:"pipelines"`
		} `json:"project"`
	}
	if err := c.GQL(q, vars, &r); err != nil {
		return nil, err
	}
	if r.Project == nil {
		return nil, fmt.Errorf("project %s not found", path)
	}
	out := r.Project.Pipelines.Nodes
	for i := range out {
		out[i].Project = path
		out[i].WebURL = c.BaseURL + out[i].Path
	}
	return out, nil
}

func (c *Client) MRPipelines(path, iid string) ([]Pipeline, error) {
	q := `query($p: ID!, $iid: String!) { project(fullPath: $p) { mergeRequest(iid: $iid) { pipelines(first: 50) { nodes {
  id iid status ref sha source duration createdAt finishedAt path user { username } commit { title } } } } } }`
	var r struct {
		Project *struct {
			MergeRequest *struct {
				Pipelines Nodes[Pipeline] `json:"pipelines"`
			} `json:"mergeRequest"`
		} `json:"project"`
	}
	if err := c.GQL(q, map[string]string{"p": path, "iid": iid}, &r); err != nil {
		return nil, err
	}
	if r.Project == nil || r.Project.MergeRequest == nil {
		return nil, fmt.Errorf("MR !%s not found", iid)
	}
	out := r.Project.MergeRequest.Pipelines.Nodes
	for i := range out {
		out[i].Project = path
		out[i].WebURL = c.BaseURL + out[i].Path
	}
	return out, nil
}

type PipelineJobs struct {
	Status string
	Jobs   []Job
}

func (c *Client) Jobs(path, pipelineIID string) (*PipelineJobs, error) {
	q := `query($p: ID!, $iid: ID!) { project(fullPath: $p) { pipeline(iid: $iid) { status stages { nodes { name jobs { nodes {
  id name status duration allowFailure retryable cancelable playable manualJob startedAt webPath
  downstreamPipeline { id iid project { fullPath } } } } } } } } }`
	var r struct {
		Project *struct {
			Pipeline *struct {
				Status string `json:"status"`
				Stages Nodes[struct {
					Name string     `json:"name"`
					Jobs Nodes[Job] `json:"jobs"`
				}] `json:"stages"`
			} `json:"pipeline"`
		} `json:"project"`
	}
	if err := c.GQL(q, map[string]string{"p": path, "iid": pipelineIID}, &r); err != nil {
		return nil, err
	}
	if r.Project == nil || r.Project.Pipeline == nil {
		return nil, fmt.Errorf("pipeline #%s not found", pipelineIID)
	}
	res := &PipelineJobs{Status: r.Project.Pipeline.Status}
	for _, st := range r.Project.Pipeline.Stages.Nodes {
		for _, j := range st.Jobs.Nodes {
			j.Stage = st.Name
			j.Project = path
			j.WebURL = c.BaseURL + j.WebPath
			res.Jobs = append(res.Jobs, j)
		}
	}
	return res, nil
}

func (c *Client) JobStatus(path, jobID string) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	err := c.REST("GET", fmt.Sprintf("%s/jobs/%s", pp(path), jobID), nil, &r)
	return r.Status, err
}

func (c *Client) Trace(path, jobID string) (string, error) {
	var s string
	err := c.REST("GET", fmt.Sprintf("%s/jobs/%s/trace", pp(path), jobID), nil, &s)
	return s, err
}

func (c *Client) Issues(path, state string) ([]Issue, error) {
	q := `query($p: ID!) { project(fullPath: $p) { issues(state: ` + state + `, first: 100, sort: UPDATED_DESC) { nodes {
  iid title state webUrl updatedAt author { username } assignees { nodes { username } } labels { nodes { title } } userNotesCount } } } }`
	var r struct {
		Project *struct {
			Issues Nodes[Issue] `json:"issues"`
		} `json:"project"`
	}
	if err := c.GQL(q, map[string]string{"p": path}, &r); err != nil {
		return nil, err
	}
	if r.Project == nil {
		return nil, fmt.Errorf("project %s not found", path)
	}
	out := r.Project.Issues.Nodes
	for i := range out {
		out[i].Project = path
	}
	return out, nil
}

// ---------- actions ----------

func (c *Client) mr(path, iid string) string {
	return fmt.Sprintf("%s/merge_requests/%s", pp(path), iid)
}

func (c *Client) Approve(path, iid string) error {
	return c.REST("POST", c.mr(path, iid)+"/approve", nil, nil)
}

func (c *Client) Unapprove(path, iid string) error {
	return c.REST("POST", c.mr(path, iid)+"/unapprove", nil, nil)
}

func (c *Client) Merge(path, iid string, auto bool) error {
	f := map[string]string{}
	if auto {
		f["auto_merge"] = "true"
		f["merge_when_pipeline_succeeds"] = "true"
	}
	return c.REST("PUT", c.mr(path, iid)+"/merge", f, nil)
}

func (c *Client) Rebase(path, iid string) error {
	return c.REST("PUT", c.mr(path, iid)+"/rebase", nil, nil)
}

func (c *Client) SetMRState(path, iid, event string) error {
	return c.REST("PUT", c.mr(path, iid), map[string]string{"state_event": event}, nil)
}

var draftRe = regexp.MustCompile(`(?i)^\s*(\[draft\]|\(draft\)|draft:|draft\s)\s*`)

func (c *Client) SetDraft(path, iid, title string, draft bool) error {
	t := draftRe.ReplaceAllString(title, "")
	if draft {
		t = "Draft: " + t
	}
	return c.REST("PUT", c.mr(path, iid), map[string]string{"title": t}, nil)
}

// SetReviewers changes the reviewers of an MR. mode: REPLACE, APPEND or REMOVE.
func (c *Client) SetReviewers(path, iid string, usernames []string, mode string) error {
	names, _ := json.Marshal(usernames)
	q := `mutation($p: ID!, $iid: String!) { mergeRequestSetReviewers(input: {projectPath: $p, iid: $iid, ` +
		`reviewerUsernames: ` + string(names) + `, operationMode: ` + mode + `}) { errors } }`
	var r struct {
		Set struct {
			Errors []string `json:"errors"`
		} `json:"mergeRequestSetReviewers"`
	}
	if err := c.GQL(q, map[string]string{"p": path, "iid": iid}, &r); err != nil {
		return err
	}
	if len(r.Set.Errors) > 0 {
		return errors.New(strings.Join(r.Set.Errors, "; "))
	}
	return nil
}

func (c *Client) CommentMR(path, iid, body string) error {
	return c.REST("POST", c.mr(path, iid)+"/notes", map[string]string{"body": body}, nil)
}

func (c *Client) CommentIssue(path, iid, body string) error {
	return c.REST("POST", fmt.Sprintf("%s/issues/%s/notes", pp(path), iid), map[string]string{"body": body}, nil)
}

func (c *Client) SetIssueState(path, iid, event string) error {
	return c.REST("PUT", fmt.Sprintf("%s/issues/%s", pp(path), iid), map[string]string{"state_event": event}, nil)
}

func (c *Client) PipelineAction(path, id, action string) error {
	return c.REST("POST", fmt.Sprintf("%s/pipelines/%s/%s", pp(path), id, action), nil, nil)
}

func (c *Client) NewPipeline(path, ref string) (string, error) {
	var r struct {
		IID int `json:"iid"`
	}
	err := c.REST("POST", fmt.Sprintf("%s/pipeline", pp(path)), map[string]string{"ref": ref}, &r)
	return fmt.Sprint(r.IID), err
}

func (c *Client) JobAction(path, id, action string) error {
	return c.REST("POST", fmt.Sprintf("%s/jobs/%s/%s", pp(path), id, action), nil, nil)
}

func (c *Client) DefaultBranch(path string) string {
	var r struct {
		Project *struct {
			Repository *struct {
				RootRef string `json:"rootRef"`
			} `json:"repository"`
		} `json:"project"`
	}
	if err := c.GQL(`query($p: ID!) { project(fullPath: $p) { repository { rootRef } } }`, map[string]string{"p": path}, &r); err != nil {
		return "main"
	}
	if r.Project != nil && r.Project.Repository != nil && r.Project.Repository.RootRef != "" {
		return r.Project.Repository.RootRef
	}
	return "main"
}
