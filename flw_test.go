package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanTrace(t *testing.T) {
	raw := "2026-11-06T14:58:54.598016Z 00O \x1b[0KRunning with gitlab-runner\x1b[0;m\n" +
		"2026-11-06T14:58:54.598029Z 00O section_start:1762441134:prepare\r\x1b[0K\n" +
		"2026-11-06T14:58:54.598031Z 00O+progress 10%\rprogress 100%\n" +
		"plain\tline\n\n"
	got := cleanTrace(raw)
	want := []string{"Running with gitlab-runner\x1b[0;m\x1b[0m", "progress 100%", "plain    line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestColWidthsDropsColumns(t *testing.T) {
	l := &ListView{cols: []Column{{Title: "A", Width: 6}, {Title: "T"}, {Title: "X", Width: 30, Drop: 1}, {Title: "Y", Width: 10}}}
	ws := l.colWidths(60)
	if ws[2] != -1 {
		t.Fatalf("expected column X hidden, got %v", ws)
	}
	if sum := ws[0] + ws[1] + ws[3] + 2; sum != 60 {
		t.Fatalf("widths %v should fill 60, got %d", ws, sum)
	}
}

func TestShortScope(t *testing.T) {
	if got := shortScope("acme/platform/api #749"); got != "api #749" {
		t.Fatal(got)
	}
}

func TestDraftTitle(t *testing.T) {
	for _, in := range []string{"Draft: foo", "[Draft] foo", "draft: foo", "foo"} {
		if got := draftRe.ReplaceAllString(in, ""); got != "foo" {
			t.Fatalf("%q -> %q", in, got)
		}
	}
}

func TestBizDurSkipsWeekend(t *testing.T) {
	fri := time.Date(2026, 11, 6, 15, 0, 0, 0, time.Local) // Friday
	mon := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local) // Monday
	if got := bizDur(fri, mon); got != 19*time.Hour {
		t.Fatalf("got %v, want 19h", got)
	}
}

func mrWith(author string) *MR {
	return &MR{State: "opened", Author: &User{Username: author}, CreatedAt: time.Now().Add(-200 * time.Hour)}
}

func TestTurnReviewIsMine(t *testing.T) {
	m := mrWith("alice")
	m.Reviewers.Nodes = []User{{Username: "me"}}
	turn := computeTurn(m, "me")
	if !turn.Mine || turn.What != "review" {
		t.Fatalf("got %+v", turn)
	}
}

func TestTurnWaitingOnReviewerNeedsPing(t *testing.T) {
	m := mrWith("me")
	m.Reviewers.Nodes = []User{{Username: "bob"}}
	turn := computeTurn(m, "me")
	if turn.Mine || turn.What != "review" || turn.Owners[0] != "bob" || turn.Level != 2 {
		t.Fatalf("got %+v", turn)
	}
}

func TestTurnRecentPingResetsLevel(t *testing.T) {
	m := mrWith("me")
	m.Reviewers.Nodes = []User{{Username: "bob"}}
	m.Timeline.Nodes = []Note{{Author: &User{Username: "me"}, Body: "@bob ping", CreatedAt: time.Now().Add(-time.Minute)}}
	turn := computeTurn(m, "me")
	if turn.Level != 0 || turn.Pings != 1 {
		t.Fatalf("got %+v", turn)
	}
}

func TestTurnThreadsOnAuthor(t *testing.T) {
	m := mrWith("me")
	m.Approved = true
	note := func(u string) Nodes[Note] {
		return Nodes[Note]{Nodes: []Note{{Author: &User{Username: u}, CreatedAt: time.Now()}}}
	}
	m.Threads.Nodes = []Thread{{Resolvable: true, First: note("bob"), Last: note("bob")}}
	if turn := computeTurn(m, "me"); !turn.Mine || turn.What != "reply 1 thread" {
		t.Fatalf("got %+v", turn)
	}
	m.Threads.Nodes[0].Last = note("me")
	if turn := computeTurn(m, "me"); turn.Mine || turn.What != "answer 1 thread" || turn.Owners[0] != "bob" {
		t.Fatalf("got %+v", turn)
	}
}

func TestCacheable(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"api", "graphql", "-f", "query=query { currentUser { username } }"}, true},
		{[]string{"api", "graphql", "-f", "query=mutation { mergeRequestSetDraft }"}, false},
		{[]string{"api", "projects/a%2Fb/merge_requests/1/diffs", "-X", "GET"}, true},
		{[]string{"api", "projects/a%2Fb/merge_requests/1/approve", "-X", "POST"}, false},
		{[]string{"api", "projects/a%2Fb/jobs/9/trace", "-X", "GET"}, false},
		{[]string{"mr", "checkout", "1"}, false},
	}
	for _, c := range cases {
		if got := cacheable(c.args); got != c.want {
			t.Errorf("cacheable(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	old := cacheDir
	cacheDir = t.TempDir()
	defer func() { cacheDir = old }()
	args := []string{"api", "graphql", "-f", "query=query { x }", "-f", "a=1"}
	if _, _, err := cacheGet(args); err == nil {
		t.Fatal("expected a miss")
	}
	cachePut(args, []byte(`{"data":{}}`))
	b, at, err := cacheGet(args)
	if err != nil || string(b) != `{"data":{}}` || at.IsZero() {
		t.Fatalf("got %q %v %v", b, at, err)
	}
	// offline client never runs glab: a miss is an error, writes are refused
	c := (&Client{}).Cached()
	if _, err := c.glab("api", "graphql", "-f", "query=mutation { y }"); err == nil {
		t.Fatal("offline write should fail")
	}
}

func TestRexHelpers(t *testing.T) {
	if got := branchSlug("feature/ABC-12-Add_SSH-keys"); got != "abc-12-add-ssh-keys" {
		t.Errorf("branchSlug = %q", got)
	}
	if got := claudeProjectDir("/a/b.c/.claude/worktrees/x"); filepath.Base(got) != "-a-b-c--claude-worktrees-x" {
		t.Errorf("claudeProjectDir = %q", got)
	}
	f := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(f, []byte(`{"cwd":"/repo","gitBranch":"main"}`+"\n"+`{"cwd":"/repo/.claude/worktrees/x","gitBranch":"feat/x"}`+"\n"), 0o600)
	if b := transcriptBranch(f); b != "feat/x" {
		t.Errorf("transcriptBranch = %q", b)
	}
	if d := transcriptStartDir(f); d != "/repo" {
		t.Errorf("transcriptStartDir = %q", d)
	}
}

func TestReviewerConfig(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg = Config{}
	cfg.Reviewers.Default = []string{"alice", "bob"}
	cfg.Reviewers.Projects = map[string][]string{"acme": {"carol"}, "acme/api": {"dave"}}
	for p, want := range map[string]string{"other/x": "alice,bob", "acme/web": "carol", "acme/api": "dave", "acme/api2": "carol"} {
		if got := strings.Join(defaultReviewers(p), ","); got != want {
			t.Errorf("defaultReviewers(%s) = %s, want %s", p, got, want)
		}
	}
	u, err := parseUsernames("@alice, bob  @alice\tcarol.d")
	if err != nil || strings.Join(u, ",") != "alice,bob,carol.d" {
		t.Errorf("parseUsernames = %v %v", u, err)
	}
	if _, err := parseUsernames("al ice\"}"); err == nil {
		t.Error("expected an error on a bad username")
	}
	m := &MR{Author: &User{Username: "alice"}}
	m.Reviewers.Nodes = []User{{Username: "bob"}}
	if got := strings.Join(reviewersToAdd(m, []string{"alice", "bob", "carol"}), ","); got != "carol" {
		t.Errorf("reviewersToAdd = %s", got)
	}
}

func TestRexJSONArgs(t *testing.T) {
	// regression: --json was appended after "--" and reached claude
	args := []string{"window", "new", "-s", "S", "L", "--", "bash", "-lc", "x"}
	full := append([]string{}, args...)
	i := len(full)
	for j, a := range full {
		if a == "--" {
			i = j
			break
		}
	}
	full = append(full[:i], append([]string{"--json"}, args[i:]...)...)
	if got := strings.Join(full, " "); got != "window new -s S L --json -- bash -lc x" {
		t.Errorf("got %q", got)
	}
}

func TestNotifierDiff(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg = Config{}
	mk := func(pipeline string, notes ...Note) *MR {
		m := &MR{IID: "7", Title: "t", State: "opened", Author: &User{Username: "me"},
			Project: &struct {
				FullPath string `json:"fullPath"`
			}{"g/p"}, HeadPipeline: &Pipeline{Status: pipeline}}
		m.Timeline.Nodes = notes
		return m
	}
	var n notifier
	if got := n.diff([]*MR{mk("running")}, "me"); len(got) != 0 {
		t.Fatalf("first poll is a baseline, got %v", got)
	}
	note := Note{Author: &User{Username: "bob"}, Body: "please rename this", CreatedAt: time.Now()}
	got := n.diff([]*MR{mk("failed", note)}, "me")
	var kinds []string
	for _, x := range got {
		kinds = append(kinds, x.kind)
	}
	if k := strings.Join(kinds, ","); !strings.Contains(k, nComment) || !strings.Contains(k, nPipeline) {
		t.Errorf("kinds = %s", k)
	}
	if got := n.diff(nil, "me"); len(got) != 1 || got[0].kind != nMerged {
		t.Errorf("expected merged notice, got %v", got)
	}
}

func TestStatusSeq(t *testing.T) {
	s := statusSeq("p-1", "blocked", "question", "!1 p", "▶ your turn")
	if !strings.HasPrefix(s, "\x1b]7501;state=blocked:app=flw:id=p-1:kind=question:title=") || !strings.HasSuffix(s, "\x1b\\") {
		t.Errorf("bad sequence %q", s)
	}
}

func TestParseWhen(t *testing.T) {
	loc := time.Local
	fri := time.Date(2026, 11, 13, 15, 0, 0, 0, loc) // Friday 15:00
	cases := map[string]time.Time{
		"demain":       time.Date(2026, 11, 16, 9, 0, 0, 0, loc), // next business day
		"tomorrow 14h": time.Date(2026, 11, 16, 14, 0, 0, 0, loc),
		"lundi":        time.Date(2026, 11, 16, 9, 0, 0, 0, loc),
		"friday":       time.Date(2026, 11, 20, 9, 0, 0, 0, loc), // the next one, not today
		"2h":           time.Date(2026, 11, 13, 17, 0, 0, 0, loc),
		"14:00":        time.Date(2026, 11, 14, 14, 0, 0, 0, loc), // passed today: tomorrow
		"16h30":        time.Date(2026, 11, 13, 16, 30, 0, 0, loc),
		"3d":           time.Date(2026, 11, 18, 9, 0, 0, 0, loc),
		"19/11":        time.Date(2026, 11, 19, 9, 0, 0, 0, loc),
		"2026-12-01":   time.Date(2026, 12, 1, 9, 0, 0, 0, loc),
	}
	for in, want := range cases {
		got, err := parseWhen(in, fri)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseWhen("blah", fri); err == nil {
		t.Error("expected an error")
	}
}

func TestSnoozeBucket(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	loadSnoozes()
	m := &MR{IID: "1", State: "opened", Author: &User{Username: "me"}, Project: &struct {
		FullPath string `json:"fullPath"`
	}{"g/p"}}
	if b := computeTurn(m, "me").Bucket(); b == bucketSnoozed {
		t.Fatal("not snoozed yet")
	}
	setSnooze(m, time.Now().Add(time.Hour))
	if b := computeTurn(m, "me").Bucket(); b != bucketSnoozed {
		t.Fatalf("bucket = %d", b)
	}
	setSnooze(m, time.Now().Add(-time.Minute))
	if due := dueSnoozes(time.Now()); len(due) != 1 {
		t.Fatalf("due = %v", due)
	}
	if b := computeTurn(m, "me").Bucket(); b == bucketSnoozed {
		t.Fatal("should be awake")
	}
}
