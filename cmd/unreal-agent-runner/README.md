# unreal-agent-runner

Run an AI agent from a prompt or JSON request. It writes events to stdout as
JSONL and exits when the task finishes.

Install with Go 1.27+:

```sh
go install github.com/unreallabsai/unreal-agent/cmd/unreal-agent-runner@latest
```

Set an OpenAI API key and run a prompt in the current directory:

```sh
export OPENAI_API_KEY="..."
unreal-agent-runner -p 'Inspect this project and explain how to run its tests.'
```

Or run from source at the repository root:

```sh
go run ./cmd/unreal-agent-runner -p 'Inspect this project and explain how to run its tests.'
```

Choose a workspace and save the output:

```sh
unreal-agent-runner -workspace ./my-project -p 'Summarize this project.' > run.jsonl
```

Sessions: `${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/sessions`
(override with `-session-directory`). A request's `session_id` creates the
session or continues it; add `"resume": true` when the caller means to continue
one, so that a session whose files are gone fails with an `open session` error
instead of silently starting over under the same id.

You can also pass a JSON request as an argument or through stdin:

```sh
unreal-agent-runner '{"prompt":"Summarize this project."}'
unreal-agent-runner < request.json
```

OpenAI is the default provider. Set `UNREAL_HARNESS_LLM_PROVIDER` to `openai`,
`openai-codex`, `openrouter`, `fireworks`, or `ollama`, and
`UNREAL_HARNESS_LLM_MODEL` to choose a model.

Requests without `thinking_level` use `UNREAL_HARNESS_LLM_THINKING_LEVEL`, or
`high` when it is unset. Besides `low`, `medium`, `high`, `xhigh` and `max`, a
provider-specific level (for example `minimal` or `none`) is sent to the
provider as is, so a server that supports only some levels can be given one it
accepts.

To send fields beyond the standard request, such as sampling or chat-template
parameters that an OpenAI-compatible server accepts, set
`UNREAL_HARNESS_LLM_EXTRA_BODY` to a JSON object; its fields are added to every
Responses API request. Fields the harness sets itself (`model`, `input`,
`tools`, `reasoning`, `stream`, `store`, `include`, `prompt_cache_key`) cannot
be replaced:

```sh
export UNREAL_HARNESS_LLM_EXTRA_BODY='{"temperature":0.6,"top_p":0.95}'
```

While the model generates, the runner also writes its output as it streams, so
a caller can show a turn before it completes:

```json
{"type":"model_delta","turn_id":"…","attempt":1,"output_index":1,"channel":"text","text":"Hello"}
```

`channel` is `text` or `reasoning`. Consecutive deltas are merged into one
event every 250 ms (`-model-delta-interval`; `0` writes each one). They are a
preview only: they are not stored in the session or the `-log-directory` log,
and the turn's `model_response` item still carries the complete output. A
higher `attempt` means the request was retried and the turn's output starts
over. Send `"include_partial_messages": false` to write session items only.

The runner loads the workspace's `.env` file into its environment before it
reads these settings, without overriding variables that are already set. When
the workspace content is not trusted, pass `-no-workspace-dotenv` or set
`UNREAL_HARNESS_NO_WORKSPACE_DOTENV=1`; a file that could otherwise point
`UNREAL_HARNESS_LLM_BASE_URL` or a proxy variable elsewhere is then ignored.

## MCP servers

`-mcp-config FILE` offers the tools of MCP servers that speak the streamable
HTTP transport. Each server tool becomes a function tool of its own, named
`mcp__<server>__<tool>`: characters other than ASCII letters, digits, `_` and
`-` become `_`, and a name longer than 64 characters is shortened and ends in
a hash. The file uses the `mcpServers` layout that other MCP clients read:

```json
{
  "mcpServers": {
    "docs": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer ${DOCS_MCP_TOKEN}" },
      "timeout": 120000
    }
  }
}
```

- `${NAME}` in `url` or in a header value is replaced with the environment
  variable `NAME` when the runner starts, and an unset or empty variable is an
  error. This keeps credentials out of the file and off the command line.
- `timeout` is in milliseconds (default 300000, at most 3600000) and bounds
  each tool call. Starting a server (initialize and tools/list) is bounded by
  that timeout or one minute, whichever is shorter.
- Only streamable HTTP servers are supported (`"type": "http"`, the default);
  `stdio` and legacy `sse` servers are rejected.
- The runner connects to all servers when it starts. A server that fails is
  reported on stderr as `tool error> MCP server "name": ...` and left out; the
  run continues with the other tools.
- Tool calls run in the background like Bash commands. Text content is
  returned as is, and structured content as JSON unless a text block already
  carries it. Images, audio and binary resources are saved under the
  session's operation directory, and the result names their paths. A result
  longer than the output limit is saved there in full as well; the model sees
  its head and tail, with the path.
- A call that was in flight when the runner stopped is not sent again when
  the session resumes: it ends with an error saying its outcome is unknown.
  Calls recorded in a session remain readable when a later run has no MCP
  servers.
- `disallowed_tools` accepts MCP tool names.

Run `unreal-agent-runner -h` for options and the JSON request fields.

## Docker

The `unrea1labs/unreal-agent` image supports Linux on AMD64 and ARM64. Run it
with a project mounted as the workspace:

```sh
docker run --rm -i --user "$(id -u):$(id -g)" \
  -e OPENAI_API_KEY -v "$PWD:/workspace" \
  -v unreal-agent-state:/state \
  unrea1labs/unreal-agent:latest -p 'Summarize this project.'
```

Each release also publishes its Git tag (for example, `v0.1.0`) for version pinning.
