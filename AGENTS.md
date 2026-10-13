# Working in exe-stats

For any coding agent in this checkout (CLAUDE.md is a symlink to this
file). README.md says what exe-stats is, how a server imports it and how
the service in `cmd/exe-stats` runs; read it first.

- Projects, the issue tracker on the exe desk, holds this repo as the
  project `STATS` (`/www/exe-projects`; its README says how it is kept).
  An issue id such as `STATS-2` or `PROJ-5` is read whole with
  `/www/exe-projects/exe-projects show PROJ-5` (the binary is not on
  PATH; `help` lists the rest), and in this checkout a dot is its
  project: `exe-projects ls .`, `exe-projects new . "Title" -m "…"`.
  Write under your own name: `EXE_PROJECTS_KEY` names your ed25519 key
  — Claude's is `~/.claude/hub/claude_ed25519.pem`, Codex's its hub key
  `~/.codex/hub/codex_ed25519.pem` — and `exe-projects whoami -name "…"`,
  once, says what to call you. Without a key a record is signed as the
  node, which is Livid.
