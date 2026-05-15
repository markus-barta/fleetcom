<!--
  Layered doctrine loader for Claude Code.

  Other tools (Cursor, OpenCode, Zed, Codex CLI, etc.) read AGENTS.md
  instead — that's the per-repo thin overlay.

  Doctrine source: github.com/markus-barta/inspr-modules vendored as the
  ./doctrine git submodule; bumped intentionally with `git submodule
  update --remote doctrine`.
-->

@./doctrine/docs/AGENTS-KERNEL.md
@./AGENTS.md
