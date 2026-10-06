# Promoting the Kwerft Medium posts

*6 October 2026 · status: proposal · owner: Enzo. Covers the five drafts in
this folder. Same voice and limits as the Hatchure posts
(`hatchure-ios/docs/marketing/medium/medium-promotion-2026-09-29.md`): answer
as Enzo, never as "we", no paid reach, no paywall, no trackers.*

| File | Post | Words |
| --- | --- | --- |
| `medium-drain-2026-10-06.md` | Zero dropped requests during a Kubernetes rollout: the case for a preStop sleep | 2,970 |
| `medium-migration-2026-10-06.md` | Moving a real production stack from Docker Compose onto my own Kubernetes platform | 2,810 |
| `medium-impersonation-2026-10-06.md` | Let Kubernetes RBAC be your app's authorization layer: impersonation in Go | 3,270 |
| `medium-upgrades-2026-10-06.md` | Self-upgrading software: the watcher has to be the old version | 3,180 |
| `medium-backups-2026-10-06.md` | Encrypting Velero backups on object storage that only offers SSE-C | 3,000 |

Images are in `medium-images/`, five or six per post:

- **A cover** (`cover-<post>.png`, 1440×756) right under the subtitle. In
  Medium's editor, make it the story's featured image (Story settings ›
  Preview image) so feeds, Hacker News and social previews show it.
  `cover-figures.mjs`.
- **Diagrams** (`<prefix>-figN-*.png`): one `<prefix>-figures.mjs` per post
  on the shared `figures-lib.mjs` (the Hatchure posts' style), rendered with
  `node render.mjs <prefix>-`.
- **Console screenshots** (`<prefix>-shot-*.png`): crops of the cards a post
  talks about, from the v0.6.0-rc.3 console with the homepage's demo data (a
  fictional web shop; no real server, name or address). The rolled-back
  upgrade in the upgrades post is a fixture with the runner's own wording.
  `console-shots.mjs` (its header says how to build the console from a tag;
  it uses the mock and Playwright from `../kwerft-homepage/tools/screenshots`).
  Re-take them when the UI changes before a post goes out.

Paste each image's alt text (the `![…]` text) into Medium's alt text field;
the italic line under a screenshot is its caption.

---

## The key calls

- **Two posts now, three after v0.6.0.** The drain and migration posts teach
  something that holds for any Kubernetes setup, and what they describe is
  already installable from the release candidates. The RBAC, upgrade and
  backup posts describe Phase 6 work (SecretSets, upgrades from the console,
  encrypted backups) that is only in v0.6.0-rc.x, while kwerft.dev still
  installs v0.4.0. A reader who tries what the post describes must get it, so
  those three wait for the stable v0.6.0. Two of them also wait for a real
  run of what they describe, because each says honestly that it has only
  passed against fakes so far:
  - **Upgrades:** one successful console upgrade on the production clusters
    from rc.3 on (the AppArmor fix the post describes is in v0.6.0-rc.3, and
    servers on rc.2 or earlier need one `install.sh` run first). Then update
    the post's "only against fakes" paragraph with what happened.
  - **Backups:** one backup to a real Hetzner bucket and a full restore onto
    a new server, which is Phase 6's exit criterion anyway. The post's "Not
    yet proven" section changes with it. Whether Hetzner accepts SSE-C on
    the last step of a multipart upload is still open.
- **Thursdays, not Tuesdays.** The Hatchure posts own Tuesdays (outbox 27
  October, Supabase 3 November) and the iPhone release is 22 October. Two
  launches in one day split your attention exactly when threads need
  answers.
- **All five to Level Up Coding.** They are engineering stories from personal
  experience with the product named once at the top and once at the end,
  which is what its guidelines ask for. Unlike TrailPhysics, none of them is
  a Show HN: Kwerft's own Show HN belongs to the public beta (see §4), and
  these posts are what it can point back to.
- **The front door is the repository, and it isn't ready yet.** The README
  still says "Status: Phase 0 (foundations)", the repository has no
  description, no topics and no open issues. A visitor from Hacker News gives
  it ten seconds. §1 comes before any post.
- **Disclose the AI assistance.** Each draft carries one disclosure line in
  its first paragraphs, as Medium requires for AI-assisted text. Rewriting a
  post in your own words is what makes it eligible for Boost and General
  distribution (§5).
- **The utm tags count nothing today.** kwerft.dev has no analytics, by
  choice (its privacy page says so). Keep the tags: they cost nothing, and
  they start counting if you ever turn on Netlify's server-side analytics.
  Measure with Medium's referrers, GitHub traffic and GHCR pulls instead
  (§6).

## 1. Before the first post: the repository (by Monday 12 October)

In rough order of how much each matters:

1. **Rewrite the README's status line.** "Phase 0 (foundations) … most console
   features are still to be built" was true on 4 October. Replace it with what
   is true now: v0.4.0 stable, v0.6.0 in release candidates, running a real
   production workload (Hatchure's router and tiles) on two clusters. Add one
   console screenshot from `../kwerft-homepage` and the install command.
2. **Description, homepage and topics.** Description: "Kubernetes console for
   Hetzner servers, installed with one script." Homepage: https://kwerft.dev.
   Topics: kubernetes, k3s, cilium, hetzner, hetzner-cloud, self-hosted, paas,
   gitops, golang, react. Without topics the repository can't be found
   through GitHub's topic pages.
3. **CI badges** for `ci.yml` and `e2e.yml` at the top of the README. "Every
   release passes a fresh install and an upgrade on real servers" is only
   believable next to a green check.
4. **Open three or four honest issues** from `docs/plan.md` › Open follow-ups,
   for example: the registry has no per-project credentials; Apps without a
   health check are Ready before they listen (the drain post mentions it);
   `VolumeFillingUp` never fires on local-path volumes; Slack not yet tried as
   a channel. Label one or two `good first issue` only once the CLA exists;
   until then say in each that pull requests wait for the CLA.
5. **Turn on GitHub Discussions** with a Q&A category. Questions from the
   posts ("does this work on other providers?") should land somewhere that
   isn't an issue.
6. **Link each post from the README** once it's live, under a short "Writing"
   section, and from kwerft.dev's docs where a post explains a page (drain →
   App settings, backups → Backups).

## 2. The schedule

| Date | Post | Where, besides Medium |
| --- | --- | --- |
| Fri 9 Oct | Submit the drain and migration drafts to Level Up Coding (§5) | |
| **Thu 22 Oct** | Zero dropped requests during a Kubernetes rollout: the case for a preStop sleep | Hacker News (ordinary submission), r/kubernetes, r/golang (for the shutdown section), KubeWeekly and DevOps Weekly submissions |
| **Thu 29 Oct** | Moving a real production stack from Docker Compose onto my own Kubernetes platform | r/selfhosted, r/hetzner, Hacker News |
| v0.6.0 + 1 week | Let Kubernetes RBAC be your app's authorization layer: impersonation in Go | r/kubernetes, r/golang, Golang Weekly, KubeWeekly |
| + 2 weeks | Self-upgrading software: the watcher has to be the old version | Hacker News, r/devops, r/selfhosted |
| + 3 weeks | Encrypting Velero backups on object storage that only offers SSE-C | r/kubernetes, r/hetzner, Velero's community (§3) |

The drain post goes first because it has the widest audience and needs
nothing from Kwerft to be useful. The migration post follows because it is
the "why trust this" story: a real stack, moved, with the numbers. If Level
Up Coding schedules either later than the date above, take its date: nothing
else depends on these.

Each post closes by pointing to the ones already out (add the links when
publishing; the drafts don't have them yet).

## 3. Per post: the angle for each community

One place a day, each with its own text, never the same paragraph pasted
around. Read each subreddit's self-promotion rules that week; several keep it
to a weekly thread.

- **Drain.** Hacker News and r/kubernetes: the measurement leads ("120 of 667
  requests answered 503 within a second of the new pod turning Ready; with a
  5-second preStop sleep, 0 of 1,881"). The question to ask: *what does your
  setup do between SIGTERM and the endpoint update?* r/golang: only the
  graceful-shutdown section, as a question about `http.Server.Shutdown` and
  keep-alive connections.
- **Migration.** r/selfhosted is the best fit of all five posts: Compose to
  Kubernetes, crontab to schedules, a 350 GiB volume, DNS as the rollback.
  Be upfront that it's your own platform in the first line, and that the new
  setup renders tiles slower (the post says so).
- **RBAC.** r/kubernetes and r/golang: impersonation is under-used and the
  isolation test suite is the part people will want to copy. The patch-
  response leak in the secrets section is the detail that gets quoted.
- **Upgrades.** Hacker News: "the watcher is always the old version" is a
  general pattern (self-updating agents, installers, appliances). r/selfhosted:
  "upgrade from the browser with rollback" is what they ask every platform
  for.
- **Backups.** r/hetzner and anyone running Velero on S3-compatible storage.
  If the post's finding holds (the AWS plugin's key file silently cut to 32
  bytes), **open an issue upstream on `vmware-tanzu/velero-plugin-for-aws`
  before the post goes out**, describe it neutrally, and link the issue from
  the post. A useful upstream report is worth more than the post, and it
  makes the post's claim checkable. Hetzner's community tutorials site takes
  how-to submissions; a step-by-step version of the SSE-C setup would fit
  there, but check its terms on content published elsewhere first.

## 4. Where Kwerft's own launch fits

These posts are not the launch. The launch is a **Show HN on the day of the
public beta** (Phase 6's exit criterion: a full restore onto a new server),
linking kwerft.dev with the install command, with a first comment of five or
six lines: what it is, the 2.4 GB memory budget on an 8 GB server, that a
real production stack runs on it, links to two of these posts, and one
specific question (*what would stop you running this on your own Hetzner
box?*). The posts already out by then give the thread something to read
beyond the homepage.

## 5. Level Up Coding: submission and AI disclosure

**How.** If Level Up Coding already added you as a writer for the outbox and
Supabase posts, submit each draft from Medium's editor. Otherwise, one email
to submit@gitconnected.com with the first two draft links and a sentence
saying three more follow after a release. Submit as unpublished drafts, not
member-only. No Medium referral links. If a post isn't accepted within two
weeks of submitting, publish it under your own name on the planned date.

**AI disclosure.** The drafts were written with Claude from Kwerft's docs,
code and git history. Medium's rules (checked 29 September for the Hatchure
posts): AI-assisted text must be disclosed, undisclosed it gets only Network
distribution, and Boost requires human-created writing. Each draft carries:

> *I drafted this post with the help of an AI writing assistant. The system,
> the bugs and the numbers are Kwerft's own.*

Same two honest options as for Hatchure: publish with the line, or rewrite in
your own words (then delete the line). Rewriting matters most for the drain
and migration posts, where Medium's own distribution decides the reach.
Check Level Up Coding's current guidelines on AI-assisted writing when
submitting; Medium's rule applies either way.

### The notes to the editors

Adjust the first line to the publication.

**Zero dropped requests during a Kubernetes rollout: the case for a preStop
sleep** (about 3,000 words, 11 minutes):

> Hi, I'd like to offer this piece to Level Up Coding. It's about why a
> Kubernetes rolling update drops requests even with maxSurge 1,
> maxUnavailable 0 and an honest health check, with a real measurement: 120
> of 667 requests answered 503 within a second of the new pod turning Ready,
> and 0 of 1,881 after the fix. It walks through the fix in Go and YAML (the
> kubelet's own preStop sleep, which works on distroless images, and a grace
> period that accounts for it), says what the fix does not cover, and ends
> with how a Go server should shut down. It links once to the open-source
> platform it comes from. It isn't published anywhere else, and it isn't
> member-only. Thanks for reading it.
> Enzo Hilzinger

**Moving a real production stack from Docker Compose onto my own Kubernetes
platform** (about 2,800 words, 11 minutes):

> Hi, I'd like to offer this piece to Level Up Coding. It's a field report
> on moving a real backend (a routing stack, eight cron jobs and a quarter
> of a terabyte of map tiles) from one Docker Compose box onto Kubernetes,
> written by the author of the platform it moved onto. Each section is one
> thing the move broke, with the real YAML and Go: requests dropped on
> restart, a job that needed more than 30 seconds to stop, ssh key
> permissions on mounted Secrets, and a CSI quirk where one read-only mount
> made a whole disk read-only. It closes with the cutover pattern (run both,
> compare, move one DNS record) and rules that hold for any Compose move.
> It links to the platform and to the app it serves. It isn't published
> anywhere else, and it isn't member-only. Thanks for reading it.
> Enzo Hilzinger

**Let Kubernetes RBAC be your app's authorization layer: impersonation in
Go** (about 3,300 words, 12 minutes):

> Hi, I'd like to offer this piece to Level Up Coding. Most Kubernetes
> dashboards reimplement permissions in the application. This one shows,
> with real Go and RBAC, how to impersonate each user so the API server
> decides instead, the one place I deliberately don't, and the test suite
> against a real API server that keeps both paths honest. It ends with a
> less-known leak: a patch-only Secret isn't write-only, because the PATCH
> response carries the values, and how metadata-only responses fix that.
> It says what the design does not cover, including a flaw that writing it
> turned up. It links once to the open-source project it comes from. It
> isn't published anywhere else, and it isn't member-only. Thanks for
> reading it.
> Enzo Hilzinger

**Self-upgrading software: the watcher has to be the old version** (about
3,200 words, 12 minutes):

> Hi, I'd like to offer this piece to Level Up Coding. It's about building
> an Upgrade button whose handler is the program being replaced: the old
> version watches the new one, the installer runs as a host systemd unit
> that outlives every pod, progress lives in a Kubernetes object, schema
> compatibility is checked when a release is cut, and Kubernetes itself is
> deliberately never rolled back automatically. It ends with the first real
> upgrade, which failed before it changed anything, and what that taught
> about fixing the watcher. The rules apply to any self-updating system. It
> links once to the open-source project it comes from. It isn't published
> anywhere else, and it isn't member-only. Thanks for reading it.
> Enzo Hilzinger

**Encrypting Velero backups on object storage that only offers SSE-C**
(about 3,000 words, 11 minutes):

> Hi, I'd like to offer this piece to Level Up Coding. Most Velero setups on
> S3-compatible storage write every Kubernetes Secret into the bucket in
> readable form. This piece shows how one recovery key, through HKDF-SHA256,
> encrypts Velero's objects, Kopia's volume data and etcd snapshots on
> storage that only offers SSE-C, with the Go and bash code, a detail of
> Velero's AWS plugin that silently shortens a key file, and a section that
> separates what was read in source code from what tests prove. It links
> once to the open-source project it comes from. It isn't published
> anywhere else, and it isn't member-only. Thanks for reading it.
> Enzo Hilzinger

## 6. Measuring it

Every Monday, in the same sheet as the Hatchure figures:

| What | Where | Note |
| --- | --- | --- |
| Reads, read ratio, referrers per post | Medium stats | which channel brought readers who finished |
| Visitors and referrers | GitHub › Insights › Traffic for `kwerft` | **kept only 14 days**: copy every Monday |
| Image pulls | GitHub › Packages › `kwerft` (per version) | the closest thing to an install count; subtract your own servers and the e2e runs |
| Stars, issues, discussions | the repository | issues from people who aren't you are the signal |
| Support requests | kwerft.dev/support (Netlify form) | the Kwerft Cloud demand test from `docs/plan.md` |

What "it worked" looks like after the five posts, as a guess rather than a
target: a few dozen stars, a handful of outside issues or discussions, a few
installs that aren't yours, and one support or hosting enquiry.

## 7. What not to do

- No paid promotion, no vote requests, no identical cross-posting on the
  same day.
- No claims the posts can't back: Kwerft runs one production workload, not
  "production-proven". Its HA, node-loss and second-cluster exit criteria on
  real Cloud servers are still open (`docs/plan.md` › Phase 5).
- Don't publish a post whose feature isn't in the release kwerft.dev
  installs, unless the post says plainly that it's in a release candidate.
- No IP addresses, hostnames of private servers or screenshots with real
  data. The homepage's mock-API screenshots are the ones to use.

## 8. Open decisions

1. Publish the drain and migration posts from the release candidates (as
   proposed), or hold all five for v0.6.0?
2. Each post: the disclosure line, or a rewrite in your own words (§5)?
3. The backup post's upstream issue on the Velero AWS plugin: open it
   yourself before publishing (recommended), or leave the finding in the
   post only?
4. Netlify Analytics (server-side, no cookies) for kwerft.dev, so the utm
   tags count, or stay with no analytics at all?
