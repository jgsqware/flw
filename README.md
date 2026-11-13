# flw

A keyboard-driven GitLab TUI (k9s / lazygit style) on top of [`glab`](https://gitlab.com/gitlab-org/cli).
All data goes through `glab api`, so it reuses your glab login and host.

Needs a logged-in `glab` (`glab auth login`). Get a binary from the
[releases](https://github.com/jgsqware/flw/releases) (linux/macOS, amd64/arm64), or build it with Go 1.27+:

```sh
go install github.com/jgsqware/flw@latest
flw update                 # install the latest release over the current binary
flw                        # inside a GitLab repo: its merge requests; elsewhere: your MR dashboard
flw -R group/project       # a given project
flw -host gitlab.example.com
```

## Views

| Command        | View                                                       |
|----------------|------------------------------------------------------------|
| `:my`          | my open MRs: review requested, authored, assigned          |
| `:mr`          | MRs of the current project (`s` cycles opened/merged/closed/all) |
| `:pl [ref]`    | pipelines of the current project                           |
| `:issues`      | issues of the current project                              |
| `:projects`    | my projects (`enter` selects one)                          |
| `:project x/y` | switch project                                             |

`enter` drills down (MR → details, pipeline → jobs → log, downstream job → child pipeline), `esc` goes back.

## Layout: desktop (default) or classic

The desktop layout follows the [tuios](https://github.com/Gaurav-Gosain/tuios) look: a dock with
workspaces on top (the dashboard, then one per project of your MRs, with ▶/⏳ badges), windows
with rounded borders and ● ● ●, an attention sidebar, and floating windows over a dimmed desktop.

- **Tiles**: the MR list is the master window; the selected MR's detail and its pipeline jobs
  follow the cursor on the right. `tab` moves the focus between windows, keys go to the focused one.
- **`f`** zooms the focused window (the list, or opens the detail / jobs full size); `esc` comes back.
- **`1`…`9`** (or `alt+1`…`9`, AZERTY too) switch workspace; each keeps its own views.
- **Mouse**: click a window to focus it, a row to select it (double-click opens), a dock pill or a sidebar line to switch; the wheel scrolls what is under the pointer. Hold shift (or option on macOS) to select text.
- **`ctrl+p`** opens the palette: workspaces, every MR of the dashboard, commands.
- **`space`** shows the keys of the focused window.

`flw -ui classic` (or `ui: classic` in the config) keeps the single-view layout.

## Whose turn is it? (NEXT column)

Every MR gets a **NEXT** cell saying who must act, what, and for how long (business time, weekends excluded):

| Cell | Meaning |
|---|---|
| `▶ you: review · 2h` (orange) | your move |
| `▶ you: reply 8 threads · 4d !` (red) | your move, and you have kept people waiting for over a day |
| `⏳ @bob: review · 5h` (blue) | waiting on someone, still fresh |
| `⏳ @bob: review · 1d · ping` (yellow) | waiting 1+ business day: time to ping |
| `⏳ @bob: review · 4d · re-ping` (red) | waiting 3+ days, or pinged a day ago with no answer |
| `· CI running` (gray) | nobody needs to act |

Steps, most blocking first: fix CI, fix conflict / rebase, address changes, reply to threads (last word is not the
author's), answer threads (last word is the author's), review (reviewer not done yet), request review, merge.
Lists are sorted by urgency, the title shows `▶ 4 on you · ⏳ 1 to ping`, and:

- `t` cycles the focus: all, on me, to ping, waiting on others
- `n` pings: opens a prefilled comment mentioning the people the MR waits on (`ctrl+s` sends)
- thresholds: `flw -ping 24h -reping 72h`

## Snooze

`z` on an MR puts it aside until a time you type: `demain` / `tomorrow` (next business day at
09:00), `lundi` / `monday`, `next week`, `2h`, `30m`, `3d` (business days), `14:00` or `14h`,
`demain 14h`, `19/11`, `2026-12-01`. Until then it shows `💤 tomorrow 09:00 · you: …` at the bottom,
leaves the "on you" / "to ping" counts and the Rex Activity panel, and its "your turn" / "ping"
notifications are held. When the time comes, flw sends `⏰ reminder: …` (desktop + Activity); if
flw was closed then, the reminder comes when you open it. `z` then enter on an empty value wakes
it up now. Snoozes live in `~/.local/state/flw/snoozes.json`; the wake-up hour is `snooze_at`.

## Merged MRs that still have local leftovers

My MRs merged or closed in the last 30 days stay on the dashboard while something is left of
them locally: a Rex tab (often with its Claude session), a worktree, or the main checkout still
on their branch. NEXT shows `🧹 merged · tab+claude, worktree`; `t` has a "to clean up" focus.

`X` cleans up after a confirmation that lists each step: close the tabs (never flw's own), remove
the worktree (kept if it has uncommitted changes), delete the local branch (only when its tip is
the MR's last commit, so nothing is lost even after a squash merge). `w` still jumps to the tab.

## Notifications and Rex Activity

While flw is open it checks your MRs every minute (from the cache when the daemon refreshed it,
else live) and tells you what changed since the previous check:

| Event | Example |
|---|---|
| `comment` | 💬 @bob: please rename this |
| `pipeline` | ✅ pipeline passed / ❌ pipeline failed |
| `approval` | 👍 approved by @bob / ✋ @bob requested changes |
| `turn` | ▶ your turn: merge |
| `ping` | ⏳ @bob: review, time to ping again |
| `conflict` | ⚠ conflict with main |
| `merged` | no longer open (merged or closed) |

- **Desktop popup** through the terminal (OSC 777: Rex, Ghostty, WezTerm, foot…).
- **Rex Activity panel** (OSC 7501): a summary line plus one line per MR on you (blocked),
  to ping (idle), or changed in the last 2 hours (done, or error for a failed pipeline).
  flw clears its lines when it quits. A line about an MR that has its own Rex tab is written in
  that tab's terminal, so clicking it in Activity opens the MR's tab, not flw.

Tune it in the config (`notify:` — `enabled`, `desktop`, `activity`, `every`, `events`).

## Config

`flw config` shows `~/.config/flw/config.yaml` (created from a commented template on first use),
`flw config edit` opens it in `$EDITOR`. Every field is optional; flags and env vars win.

```yaml
reviewers:
  default: [alice, bob]          # suggested by v, added by V (the MR author is skipped)
  projects:                      # per project or group; the longest match wins
    my-group/my-project: [carol]
ping: 24h                        # business time before "ping", then "re-ping"
reping: 72h
code_roots: [~/src]              # where your clones live (for w)
```

**Reviewers on an MR:** `v` opens a prompt with the current reviewers (or your defaults when there
are none); edit and press enter to replace them, empty removes them all. `V` adds your default
reviewers after a confirmation.

## Rex: one tab per MR, with its Claude Code session

Inside [Rex](https://rex.dev), `w` on an MR takes you to where it is worked on:

1. **A tab already on it** (in any Rex session): a tab named `!<iid> …`, a Claude Code session
   whose transcript is on the MR branch, or a terminal sitting in its worktree. flw switches to it.
2. **Otherwise**, in the repo's session (labelled after the repo, created if missing), it opens a
   tab `!<iid> <branch>`:
   - **past Claude session on that branch** → `claude --resume <id>`;
   - **none** → worktree `<repo>/.claude/worktrees/<branch>` (created if needed) and a new
     Claude session primed with the MR (title, URL, branches, next step, `glab mr view`).

The local clone is found automatically: `<root>/<group>/<project>` or `<root>/<project>` under
`~/src`, `~/code`, `~/git`, `~/dev`, `~/work`, `~/projects`, `~/repos`, `~/workspace` or `~`, then any
folder open in a Rex tab, then a shallow search of those roots. A folder only counts if its git
remote is the MR's project. `FLW_CODE_ROOTS=~/a:~/b` replaces the roots. Switching the app's session needs **Remote Control**
on in Rex's settings (Rex Server); without it the tab is prepared and flw tells you where.

## Cache and background service

Every read call to GitLab is saved in `~/.cache/flw/`. Each view first draws the cached copy
(title shows `⟳ cached 2m`), then refreshes from GitLab in the background. Writes and job logs
are never cached. `FLW_NO_CACHE=1` turns it off.

To keep the cache warm even when flw is closed, run the daemon as a systemd user service:

```sh
flw service install [-every 2m] [-R group/project]...   # write the unit, enable, start
flw service status | logs | uninstall
```

Each round fetches: your MRs, the opened MRs of their projects, the details of MRs that changed
since the last round, and (every 10th round) the project list. Run it in the foreground with
`flw daemon -every 2m`. After rebuilding the binary, run `flw service install` again to restart it.

## Keys

**Global**: `:` command (tab completes) · `/` filter (search in text views, `n`/`N`) · `esc` back · `q` back/quit ·
`ctrl+r` refresh · `?` help · `j/k g/G ctrl+d/u` move.

**Merge request** (list and details): `d` diff · `p` pipelines · `J` head pipeline jobs · `a`/`A` approve/revoke ·
`m` merge · `M` auto-merge when pipeline succeeds · `r` rebase · `c` comment (`ctrl+s` sends) · `D` toggle draft ·
`x` close/reopen · `n` ping · `t` focus · `C` checkout locally · `o` browser · `y` copy URL · `S` system notes (details).

**Diff**: `]`/`[` next/previous file · `w` wrap · `o` browser · `y` copy URL.

**Pipelines**: `enter` jobs · `r` retry · `x` cancel · `n` run new pipeline · `o` · `y`.

**Jobs**: `enter`/`l` log · `r` retry · `p` play manual job · `x` cancel · `R` retry the pipeline · `o` · `y`.

**Log**: auto-follows running jobs · `f` follow · `w` wrap · `h/l` scroll sideways · `/` search · `o` · `y`.

`y` copies the browser link of what is selected (MR, diff, pipeline, job, project, issue) to the clipboard
(pbcopy, wl-copy or xclip, else OSC 52 so it also works over SSH).

## Updates

While open, flw checks GitHub for a newer release (at start, then every 6 h) and shows `⬆ v0.3.0` in the
dock. `:update` (also in `ctrl+p`) downloads the archive for your OS/arch, checks it against
`checksums.txt`, replaces the binary and restarts flw in place. Outside the app: `flw update`.
Turn the check off with `update: {check: false}` in the config or `FLW_NO_UPDATE_CHECK=1`.
Builds without a release version (`go install` from a commit, local builds) are not nagged, but `:update` still works.

Releasing: `git tag v0.2.0 && git push origin v0.2.0`; the GitHub workflow runs the tests and goreleaser.

Destructive actions (merge, close, cancel, play) ask for confirmation. Lists refresh on their own
(faster while pipelines run).

## Layout

- `gitlab.go`: glab client, GraphQL/REST queries and actions
- `listview.go`: generic filterable table (responsive columns)
- `pager.go`: scrollable text view (details, diff, logs)
- `resources.go`: the views and their key actions
- `app.go`: shell (header, breadcrumbs, command bar, dialogs, help)
