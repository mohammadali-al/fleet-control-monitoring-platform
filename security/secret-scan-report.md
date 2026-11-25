# Secret / sensitive-data scan report

**Date:** 2026-11-25 · **Trigger:** repo made public · **Result:** ✅ **No secrets found.**

## Scope & tools
- Full git history: 177 commits (140 non-merge), 673 unique blobs across all refs + working tree.
- **gitleaks v8** (`detect --redact --log-opts=--all`, `dir`), **trufflehog v3**
  (`git file://.` + `filesystem`, verified+unverified), and targeted full-history greps for private
  keys, Authelia secrets, password literals, AWS/GitHub/Slack/API token prefixes, credential URLs,
  `.env` contents, emails, and machine IPs. Scanner tools were installed to a temp dir; nothing was
  written into the repo by the scan.

## Findings
**No real credentials, private keys, API/deploy tokens, Authelia secrets, or `.env` leaks** in any
commit or the working tree. Scanner hits were all false positives / synthetic fixtures:
- Minified vendored `web/assets/vendor/xterm/xterm.js` (generic-api-key false positives).
- Synthetic test PEM + throwaway password in `internal/backup/backup_test.go`, `internal/onboard/ssh_test.go`.
- Placeholder hash/email in `deploy/authelia/users.yml.example`.

Informational (not credentials, no action required):
- An internal **RFC1918** lab IP appears in generated `workflow/generated/CHECKS.md` (test host; no secret).
- The operator **email** appears in git commit author/committer metadata (inherent to git; not in any file).

## Gitignored-sensitive paths — history check
`data/`, `deploy/proxy/state/`, `.claude/settings.local.json`, `*.bak.md`, CA material
(`*.key/*.crt/*.pem/...`), built binaries (`bin/`), and `.env*` were **never committed**.
Exception: `.claude/worktrees/` was once staged as an empty **gitlink pointer** (commit mode 160000,
no file contents) and removed + gitignored the next commit — **no data exposed**.

## Recommendations
- **No rotation or history rewrite required** — nothing sensitive was committed.
- Optional hygiene: if desired, drop the dangling gitlink and/or scrub the internal IP from generated
  docs via `git filter-repo`; use a `noreply` commit email going forward if the public author email is a
  concern. All optional.
