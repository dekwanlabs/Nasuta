# Prompt Catalog

Nasuta keeps immutable built-in model prompts under `text/`, grouped by module
and function. The catalog embeds them into the binary with `go:embed`, so
deployments do not depend on a working directory or external prompt files.

Use:

- `prompts.Text(id)` for static text.
- `prompts.Render(id, data)` for runtime data.
- `prompts.MustRender(id, data)` only when invalid template data is a
  programming error.

Every `.txt` file must have one ID in `catalog.go`. Package initialization fails
for missing, empty, duplicate, undeclared, or invalid templates. Templates use
`missingkey=error`.

Keep user questions, retrieved evidence, tool results, database-owned role
prompts, and other runtime data out of this directory. Tool descriptions and
JSON Schema field descriptions stay with their tool definitions because they
are part of the tool API contract rather than standalone conversation prompts.
