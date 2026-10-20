# Project Standards for ChatGPT and Codex

These are the rules for running YK's open source projects in ChatGPT (a Project for planning, Codex for repo work). They update the earlier setup guide, low-maintenance-workflow-chatgpt.md. Where the two disagree, this file wins.

They come out of the first project built this way, BitSpeed (bitspeed.org, softdevstudio/bitspeed). The code and the releases were good. These things drifted from the standard and had to be fixed afterwards:

| What happened in BitSpeed | The rule now |
|---|---|
| The plan and handoff lived in AGENTS.md | CLAUDE.md holds the plan and handoff in every repo. AGENTS.md is a one-line pointer to it |
| Eleven review-session files piled up in docs/ | docs/ is reference material only. Session notes go in CLAUDE.md |
| About forty timestamped result folders | One test/results/latest folder plus one evidence folder per release |
| Releases named "BitSpeed Inspect 0.1.1" | Release titles are "{Name} vX.Y.Z" |
| A post titled "Transfer Is Available" | Every title stands on its own and names the product |
| The testing post covered one of two tools | The testing post covers every tool in the repo |
| One image reused on four posts, one post with none | Every post gets its own image |
| The project was never added to softdevstudio.com | Adding it is part of the launch |
| A draft release left behind after a failed publish | Delete failed drafts, or report them to YK the same day |

Paste sections 1 and 2 where they say. Sections 3 to 7 are the standard itself; both files point to it, so keep a copy in the ChatGPT Project's files.

---

## 1. Global Codex instructions (~/.codex/AGENTS.md)

Replace the global file from the earlier guide with this:

```markdown
# Global rules (YK)

- Read CLAUDE.md in the repo root before anything else. It is the one plan and
  handoff file for the project, shared with Claude sessions. Update it there.
  The repo's AGENTS.md only points to it; never put plan or status in AGENTS.md.
- Work on main. main is always the default branch.
- No ROADMAP, RESUME, STATUS or review-session files anywhere. Session notes and
  review summaries go in CLAUDE.md under "Where we are".
- docs/ holds reference material a user would read. Nothing else.
- Test results: scripts/record-results.sh writes test/results/latest and
  replaces it each run. Release evidence goes in one folder per release,
  named {product}-{version}-release. Git history keeps old runs.
- Never ask YK to publish a release. Releases go out through the automated
  workflow. Release titles are "{Name} vX.Y.Z" (or "{Name} {Tool} vX.Y.Z"
  when one repo ships several tools).
- If a release run fails after creating a draft or tag, fix the cause, release
  the next patch version and tell YK about the leftover draft in the same
  session. Never leave it unmentioned.
- No Word or PDF. The README is the manual; reference docs are Markdown.
- Never mention domain sale, domain value or domain moat anywhere.
- Never mix in content from earlier projects.
- Writing: plain words, varied sentence length, no em dashes, no "not X but Y",
  no rule of three, no thesaurus words.
- Answer questions; don't act on them. Ask before starting work YK didn't ask for.
```

## 2. The repo's own files (per project)

In the code repo root:

- **CLAUDE.md**: the full plan and handoff, using the template from the earlier guide (it was called AGENTS.md there; rename it). Sections: project facts, low maintenance workflow, screening record, code and completion standard, site standard, development plan, Where we are, Next.
- **AGENTS.md**: exactly one line:

```markdown
Read CLAUDE.md first. It is the only plan and session handoff file for this repository; do not add to this file.
```

Codex reads AGENTS.md automatically and Claude reads CLAUDE.md automatically, so both tools land on the same plan.

---

## 3. Code repo standard

Layout at the end of every session:

```
CLAUDE.md                 plan and handoff (the only one)
AGENTS.md                 one-line pointer
README.md                 manual and plain-language explainer
LICENSE  NOTICE  THIRD_PARTY_LICENSES.md
docs/                     reference only: formats, protocols, release notes
test/results/README.md    what each folder holds
test/results/latest/      newest recorder run, replaced each time
test/results/{product}-{version}-release/   public verification per release
.github/workflows/        ci.yml, release.yml (one per product if needed)
```

Tests and review:

- Tests for every operation, plus failure tests aimed at the product's main promise.
- CI on every push to main, Linux and macOS, with go vet and race tests.
- Before calling anything done, run an independent review as a separate Codex task that hasn't seen the work. Fix what it finds and record the results again. Put the outcome in two or three sentences in CLAUDE.md, not in a file.

Releases:

- Raise the version in the code on main. CI goes green, the release workflow builds Linux, macOS and Windows archives, smoke-tests them, tags and publishes.
- Title: "{Name} vX.Y.Z". Archive names carry no version.
- Not a pre-release. No placeholder releases, no bumps just to rerun checks.
- After publishing, check every asset against SHA256SUMS at the tag and at releases/latest, and run a downloaded binary.

## 4. Site standard

- Same Hugo Launch template as the other project sites, unchanged. Content and config values only. No new layouts, CSS, _index.md files or public/ folder.
- Pages: about, contact (info@{domain}), quick start, download, reference.
- Footer line "A SoftDevStudio.com project" linking to https://softdevstudio.com.
- Download links use releases/latest/download/{archive}. Check every one against the real release assets before pushing.

Posts:

- Launch posts: launch, plain-language explainer of the category, how it was tested, the name.
- If the repo ships more than one tool, every post that is about "the project" covers all of them. The testing post explains how each tool was tested.
- Case studies: one per tool once it's released, tagged "case studies" plus the tool's tag. Each one walks through a real run with real output, made with the released binary or the repo's saved examples. Say how it was run (machine, file size, sample count) and what the result does and doesn't prove. No invented numbers.
- Frontmatter: title in double quotes, date only (2026-10-20), draft: false, tags. Add featured_image when there's an image. No description, no categories.
- Titles: normal title casing, stand on their own, lead with the product name and say what happened. "BitSpeed Transfer Resumes Interrupted File Transfers From Verified Chunks", never "Transfer Is Available". ASCII letters, digits, spaces and hyphens only; no dots, colons or quotes, so URLs stay clean.
- Never repeat the title in the body.
- Images: every post has its own image, drawn as SVG into static/images and named after the post's subject. Use the https://{domain}/images/... address in featured_image and inline after the second or third paragraph. Never reuse one image across posts.
- Changing a published title changes its URL. Update any links to it (the code repo's scripts/check-site.py too) in the same session.

## 5. SoftDevStudio.com (part of every launch)

The studio site repo is photozeitgeist/softdevstudio.com. Read its CONTENT-PLAN.md first; it holds the rules. When a project launches:

1. Add a [[projects]] entry to data/studio.toml: name, kind, lang, line, more, site, code.
2. In CONTENT-PLAN.md, update the brief (count and list), project names, tag table, footer list, feeds table, session log and NEXT SESSION.
3. Write one short note in content/posts/: a few paragraphs in the studio's own words, linked to the project's posts and release. Tags: the project name, then "release". Titles follow the site's title rule: no dots, even in version numbers.
4. Commit, pull with rebase, push to main.

## 6. End of session checklist

Before saying a session is finished, check each line:

- [ ] CLAUDE.md "Where we are" says what changed, which checks passed and what's next. Nothing else got a status file.
- [ ] docs/ has only reference material.
- [ ] test/results has latest/ and release folders only.
- [ ] CI is green on the last commit.
- [ ] If a release went out: title is "{Name} vX.Y.Z", assets match SHA256SUMS, site download links resolve, no draft left behind.
- [ ] Site posts pass the frontmatter and title rules, each has its own image, and Hugo builds with no broken internal links.
- [ ] New project: listed on softdevstudio.com with a note.
- [ ] Report to YK in a few lines: what shipped, anything that needs his decision.

## 7. Prompts for daily use

New project, after the go-ahead:

> Read CLAUDE.md and the project standards file. Set up the repo to section 3 (CLAUDE.md plus the one-line AGENTS.md), then build to the plan. Run an independent review before calling it done. Then the site to section 4, then the softdevstudio.com entry and note to section 5. Finish with the section 6 checklist.

Standards check on an existing project:

> Read CLAUDE.md. Check the code repo, site repo and releases against sections 3 to 6 of the project standards file. List each deviation in one line. Don't change anything until I say which ones to fix.

Release:

> Raise {Tool} to vX.Y.Z on main. After the release workflow finishes, check the title, the assets against SHA256SUMS at the tag and at latest, a downloaded binary, and the site's download links. Update CLAUDE.md and report.

Case study:

> Write a case study post for {Tool} on {domain}: a real run with the released binary, real output, its own SVG image. Follow section 4.
