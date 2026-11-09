"""Fictional GitLab data for the flw site demo: company "acme", users alex (you), sam, kim, noor.

Times are given in business hours before "now" (weekends skipped), the way flw counts them,
so the NEXT column reads the same whatever day the generator runs.
"""
import datetime as dt
import os

ME = "alex"
BASE = "https://gitlab.example.com"
# fixed for one demo session (set by demo-env.sh), so times don't drift between calls
NOW = (dt.datetime.fromtimestamp(int(os.environ["FLW_DEMO_NOW"]), dt.timezone.utc)
       if os.environ.get("FLW_DEMO_NOW") else dt.datetime.now(dt.timezone.utc).replace(microsecond=0))


def bh(hours):
    """ISO time `hours` business hours ago."""
    t = NOW.astimezone()
    left = hours * 60
    while left > 0:
        t -= dt.timedelta(minutes=10)
        if t.weekday() < 5:
            left -= 10
    return t.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")


PHASE = int(os.environ.get("FLW_DEMO_PHASE", "1"))
try:
    with open(os.environ.get("FLW_DEMO_PHASE_FILE", "/nonexistent")) as f:
        PHASE = int(f.read().strip() or 1)
except OSError:
    pass


def user(u):
    return {"username": u}


def mr(project, iid, title, author, branch, created, commit, *, pipeline=("success", 2), reviewers=(),
       approved=False, left=1, status="NOT_APPROVED", conflicts=False, draft=False, threads=(),
       timeline=(), labels=(), assignees=None, desc="", discussions=(), auto=False):
    revs = []
    for name, state, ok in reviewers:
        revs.append({"username": name, "mergeRequestInteraction": {"reviewState": state, "approved": ok}})
    th = []
    for first, last, at, resolved in threads:
        th.append({"resolvable": True, "resolved": resolved,
                   "first": {"nodes": [{"author": user(first)}]},
                   "last": {"nodes": [{"author": user(last), "createdAt": bh(at)}]}})
    tl = [{"author": user(a), "createdAt": bh(at), "system": sysn, "body": body} for a, at, sysn, body in timeline]
    hp = None
    if pipeline:
        st, fin = pipeline
        hp = {"id": f"gid://gitlab/Ci::Pipeline/{9000 + int(iid)}", "iid": str(700 + int(iid) % 100),
              "status": st.upper(), "finishedAt": bh(fin) if st not in ("running", "pending") else None}
    resolvable = len(threads)
    resolved = sum(1 for t in threads if t[3])
    return {
        "iid": str(iid), "title": title, "draft": draft, "state": "opened",
        "webUrl": f"{BASE}/{project}/-/merge_requests/{iid}", "sourceBranch": branch, "targetBranch": "main",
        "updatedAt": bh(min(commit, *(t[1] for t in timeline)) if timeline else commit), "createdAt": bh(created),
        "project": {"fullPath": project}, "author": user(author), "headPipeline": hp,
        "approved": approved, "approvalsLeft": left,
        "approvedBy": {"nodes": [user(n) for n, _, ok in reviewers if ok]},
        "conflicts": conflicts, "userNotesCount": len([t for t in timeline if not t[2]]),
        "detailedMergeStatus": status, "resolvableDiscussionsCount": resolvable, "resolvedDiscussionsCount": resolved,
        "reviewers": {"nodes": revs}, "labels": {"nodes": [{"title": l} for l in labels]},
        "threads": {"nodes": th}, "timeline": {"nodes": tl},
        "commits": {"nodes": [{"committedDate": bh(commit)}]},
        "assignees": {"nodes": [user(a) for a in (assignees if assignees is not None else [author])]},
        "description": desc, "mergeUser": None, "approvalsRequired": 1 if left else 0,
        "mergeWhenPipelineSucceeds": auto, "squash": True, "shouldRemoveSourceBranch": True,
        "diffStatsSummary": {"additions": 86, "deletions": 21, "fileCount": 4},
        "discussions": {"nodes": list(discussions)},
    }


def note(a, at, body, system=False, path=None, line=None):
    n = {"author": user(a), "body": body, "createdAt": bh(at), "system": system, "position": None}
    if path:
        n["position"] = {"filePath": path, "newLine": line, "oldLine": None}
    return n


def disc(notes, resolvable=True, resolved=False):
    return {"resolved": resolved, "resolvable": resolvable, "notes": {"nodes": notes}}


DESC_412 = """Clients hammer `/search` and one noisy key can slow everyone down.

## What changes
- a token bucket per **API key** (was: per IP), 20 req/s with bursts of 40
- `429 Too Many Requests` with a `Retry-After` header
- limits configurable per plan in `config/limits.yaml`

## How to test
```
make test-ratelimit
hey -n 500 -c 50 -H "X-Api-Key: demo" http://localhost:8080/search?q=shoes
```
"""

MRS = [
    # your turn: changes requested, waiting on you for more than a business day
    mr("acme/platform/api", 412, "Rate-limit /search per API key", ME, "feat/search-rate-limit", 70, 40,
       reviewers=[("sam", "REQUESTED_CHANGES", False)],
       threads=[("sam", "sam", 30, False), ("sam", "sam", 29, False), ("sam", ME, 33, True)],
       timeline=[(ME, 40, True, "added 2 commits"), ("sam", 30, False, "Can we key the bucket by API key and fall back to the IP?"),
                 ("sam", 29, False, "The 429 should carry Retry-After, clients back off on it."),
                 ("sam", 29, True, "requested changes")],
       labels=["backend", "perf"], desc=DESC_412,
       discussions=[
           disc([note(ME, 70, "requested review from @sam", True)], resolvable=False),
           disc([note("sam", 30, "Can we key the bucket by API key and fall back to the IP when there is none?", path="internal/ratelimit/bucket.go", line=42)]),
           disc([note("sam", 29, "The 429 should carry `Retry-After`: our SDKs already back off on it.", path="internal/http/middleware.go", line=88)]),
           disc([note("sam", 34, "Is 20 req/s per key enough for the batch importer?"),
                 note(ME, 33, "Yes, the importer has its own plan with 200 req/s (see config/limits.yaml).")], resolved=True),
       ]),
    # your turn: someone asked you for a review
    mr("acme/web/storefront", 88, "Keep the cart when the session expires", "kim", "fix/cart-session", 6, 5,
       reviewers=[(ME, "UNREVIEWED", False), ("noor", "APPROVED", True)], labels=["frontend"]),
    # waiting on noor for days, already pinged once: re-ping
    mr("acme/infra/terraform", 231, "Move the staging VPC to a /20", ME, "net/staging-vpc-20", 64, 58,
       reviewers=[("noor", "UNREVIEWED", False)],
       timeline=[(ME, 60, True, "requested review from @noor"),
                 (ME, 30, False, "@noor friendly ping: could you have a look at this MR when you get a chance? 🙏")],
       labels=["infra"]),
    # approved and green: merge it
    mr("acme/platform/api", 415, "OpenTelemetry traces for the worker", ME, "feat/worker-otel", 20, 9,
       reviewers=[("sam", "APPROVED", True), ("kim", "APPROVED", True)], approved=True, left=0, status="MERGEABLE",
       labels=["observability"]),
    # failed pipeline: fix CI
    mr("acme/tools/ci-templates", 57, "Cache Go modules between jobs", ME, "ci/go-mod-cache", 8, 4,
       pipeline=("failed", 3), reviewers=[("sam", "UNREVIEWED", False)]),
    # waiting on kim, a bit more than a business day: time to ping
    mr("acme/platform/api", 409, "Retry webhooks with exponential backoff", ME, "feat/webhook-retry", 30, 27,
       reviewers=[("kim", "UNREVIEWED", False)], labels=["backend"]),
    # you opened a thread, sam answered: your turn to answer
    mr("acme/infra/terraform", 229, "Split the IAM module per account", "sam", "refactor/iam-split", 26, 12,
       reviewers=[(ME, "REVIEWED", False)], threads=[(ME, "sam", 4, False)],
       timeline=[(ME, 6, False, "Do we keep the old module as a shim for one release?"), ("sam", 4, False, "Yes, done in the last commit.")]),
    # fresh: waiting on noor since an hour, pipeline running
    mr("acme/web/storefront", 91, "Lazy-load the review widgets", ME, "perf/lazy-reviews", 2, 1,
       pipeline=("running" if PHASE == 1 else "success", 0), reviewers=[("noor", "UNREVIEWED", False)]),
    # draft by kim, you are reviewer: nothing to do yet
    mr("acme/web/storefront", 90, "Draft: product page, new layout", "kim", "feat/pdp-layout", 12, 10,
       draft=True, reviewers=[(ME, "UNREVIEWED", False)], status="DRAFT_STATUS"),
    # snoozed by you (see the state file the generator writes)
    mr("acme/infra/terraform", 233, "Bump the AWS provider to 6.x", ME, "deps/aws-6", 16, 15,
       reviewers=[("sam", "APPROVED", True)], approved=True, left=0, conflicts=True, status="CONFLICT"),
]

if PHASE >= 2:
    # what changes between two polls, for the notification capture
    m = MRS[0]
    m["timeline"]["nodes"].append({"author": user("sam"), "createdAt": bh(0), "system": False,
                                   "body": "Thanks! One last nit on the header name, then good to go."})
    m["userNotesCount"] += 1
    m3 = MRS[2]
    m3["reviewers"]["nodes"][0]["mergeRequestInteraction"] = {"reviewState": "APPROVED", "approved": True}
    m3["approvedBy"]["nodes"].append(user("noor"))
    m3["approved"], m3["approvalsLeft"], m3["detailedMergeStatus"] = True, 0, "MERGEABLE"

PROJECTS = [
    {"fullPath": "acme/platform/api", "name": "api", "description": "Public REST API", "openMergeRequestsCount": 7, "openIssuesCount": 12},
    {"fullPath": "acme/web/storefront", "name": "storefront", "description": "The shop front end", "openMergeRequestsCount": 5, "openIssuesCount": 31},
    {"fullPath": "acme/infra/terraform", "name": "terraform", "description": "AWS accounts, VPCs, IAM", "openMergeRequestsCount": 4, "openIssuesCount": 3},
    {"fullPath": "acme/tools/ci-templates", "name": "ci-templates", "description": "Shared CI components", "openMergeRequestsCount": 2, "openIssuesCount": 1},
]
for i, p in enumerate(PROJECTS):
    p.update({"webUrl": f"{BASE}/{p['fullPath']}", "lastActivityAt": bh(i * 3 + 1), "starCount": 3 - i if i < 3 else 0})


def pipelines(project, iid):
    out = []
    for k, (st, by, title, ago, dur) in enumerate([
        ("success", ME, "Return Retry-After on 429", 40, 412),
        ("failed", ME, "Key the bucket by API key", 46, 388),
        ("success", ME, "Token bucket middleware", 68, 401),
    ]):
        n = 1882 - k
        out.append({"id": f"gid://gitlab/Ci::Pipeline/{n}", "iid": str(731 - k), "status": st.upper(),
                    "ref": "feat/search-rate-limit", "sha": "9f3c2ab" + str(k), "source": "merge_request_event",
                    "duration": dur, "createdAt": bh(ago), "finishedAt": bh(ago - 1), "path": f"/{project}/-/pipelines/{n}",
                    "user": user(by), "commit": {"title": title}})
    return out


def jobs(status="SUCCESS"):
    def j(i, name, st, dur, **kw):
        d = {"id": f"gid://gitlab/Ci::Build/{51200 + i}", "name": name, "status": st.upper(), "duration": dur,
             "allowFailure": False, "retryable": st != "manual", "cancelable": False, "playable": st == "manual",
             "manualJob": st == "manual", "startedAt": bh(40), "webPath": f"/acme/platform/api/-/jobs/{51200 + i}",
             "downstreamPipeline": None}
        d.update(kw)
        return d
    return {"status": status, "stages": {"nodes": [
        {"name": "lint", "jobs": {"nodes": [j(1, "golangci-lint", "success", 48), j(2, "openapi-validate", "success", 12)]}},
        {"name": "test", "jobs": {"nodes": [j(3, "go test", "success", 131), j(4, "go test -race", "success", 186),
                                             j(5, "ratelimit-load", "success", 64)]}},
        {"name": "build", "jobs": {"nodes": [j(6, "image", "success", 95)]}},
        {"name": "deploy", "jobs": {"nodes": [j(7, "review-app", "manual", None)]}},
    ]}}


TRACE = "\n".join([
    "\x1b[0KRunning with gitlab-runner 18.4.0 (a1b2c3d4)\x1b[0;m",
    "\x1b[0K  on docker-autoscaler runner-7, system ID: s_9f8e7d6c5b4a\x1b[0;m",
    "section_start:1762700000:prepare_executor\r\x1b[0K\x1b[0K\x1b[36;1mPreparing the \"docker+machine\" executor\x1b[0;m\x1b[0;m",
    "\x1b[0KUsing Docker executor with image golang:1.24 ...\x1b[0;m",
    "section_end:1762700004:prepare_executor\r\x1b[0K",
    "section_start:1762700004:get_sources\r\x1b[0K\x1b[0K\x1b[36;1mGetting source from Git repository\x1b[0;m\x1b[0;m",
    "\x1b[32;1mFetching changes with git depth set to 20...\x1b[0;m",
    "\x1b[32;1mChecking out 9f3c2ab0 as detached HEAD (ref is refs/merge-requests/412/head)...\x1b[0;m",
    "section_end:1762700007:get_sources\r\x1b[0K",
    "section_start:1762700007:step_script\r\x1b[0K\x1b[0K\x1b[36;1mExecuting \"step_script\" stage of the job script\x1b[0;m\x1b[0;m",
    "\x1b[32;1m$ make test-ratelimit\x1b[0;m",
    "go test ./internal/ratelimit/... -run 'Bucket|Middleware' -count=1",
    "=== RUN   TestBucketPerKey",
    "--- PASS: TestBucketPerKey (0.01s)",
    "=== RUN   TestBucketFallsBackToIP",
    "--- PASS: TestBucketFallsBackToIP (0.00s)",
    "=== RUN   TestMiddlewareRetryAfter",
    "    middleware_test.go:57: 429 after 40 requests, Retry-After: 1",
    "--- PASS: TestMiddlewareRetryAfter (0.02s)",
    "=== RUN   TestPlansFromConfig",
    "--- PASS: TestPlansFromConfig (0.00s)",
    "PASS",
    "ok  \tacme/api/internal/ratelimit\t0.214s",
    "\x1b[32;1m$ hey -n 500 -c 50 -H \"X-Api-Key: demo\" http://api:8080/search?q=shoes | tail -6\x1b[0;m",
    "Status code distribution:",
    "  [200]\t40 responses",
    "  [429]\t460 responses",
    "",
    "Latency p99: 0.0183 secs",
    "section_end:1762700071:step_script\r\x1b[0K",
    "\x1b[32;1mJob succeeded\x1b[0;m",
]) + "\n"

DIFFS = [
    {"old_path": "internal/ratelimit/bucket.go", "new_path": "internal/ratelimit/bucket.go", "new_file": False,
     "deleted_file": False, "renamed_file": False, "diff": """@@ -30,12 +30,19 @@ type Limiter struct {
 	buckets sync.Map // key -> *bucket
 }

-// Allow reports whether a request from ip may go through.
-func (l *Limiter) Allow(ip string) bool {
-	b := l.bucket(ip)
+// Allow reports whether a request may go through. Requests are counted per API key,
+// or per client IP when the request has no key.
+func (l *Limiter) Allow(r *http.Request) (ok bool, retryAfter time.Duration) {
+	key := r.Header.Get("X-Api-Key")
+	if key == "" {
+		key = "ip:" + clientIP(r)
+	}
+	b := l.bucket(key, l.plans.For(key))
 	return b.take(time.Now())
 }
"""},
    {"old_path": "internal/http/middleware.go", "new_path": "internal/http/middleware.go", "new_file": False,
     "deleted_file": False, "renamed_file": False, "diff": """@@ -80,9 +80,12 @@ func RateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
 	return func(next http.Handler) http.Handler {
 		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
-			if !l.Allow(clientIP(r)) {
+			ok, wait := l.Allow(r)
+			if !ok {
+				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
 				http.Error(w, "too many requests", http.StatusTooManyRequests)
 				return
 			}
"""},
    {"old_path": "config/limits.yaml", "new_path": "config/limits.yaml", "new_file": True,
     "deleted_file": False, "renamed_file": False, "diff": """@@ -0,0 +1,8 @@
+# requests per second, per API key
+default:
+  rate: 20
+  burst: 40
+plans:
+  importer:
+    rate: 200
+    burst: 400
"""},
]
