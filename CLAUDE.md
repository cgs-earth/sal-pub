@AGENTS.md

## Claude Code specifics

- Project settings live in `.claude/settings.json` (shared) and `.claude/settings.local.json` (personal, git-ignored).
- A PostToolUse hook runs `gofmt -w` on any `.go` file Claude edits.
- `.env` and `t.json` are blocked from reads. Ask the user if you need a value from them.
- Put personal notes in `CLAUDE.local.md` (git-ignored). Shared guidance belongs in `AGENTS.md`, so every agent sees it.
