# rumbo-claude

An **external** rumbo provider that uses a Claude Pro or Max subscription.
It is a program of its own that speaks rumbo's Agent Provider Protocol,
version 1, on its stdin and stdout. It does not change rumbo, is not
registered by itself, and imports none of rumbo's `internal` packages: it
carries a copy of rumbo's protocol code, so it builds without rumbo's
repository.

## Install

It needs Go 1.27.3 or later, on Linux or macOS: it locks its credentials
with `flock`, which Windows does not have. It has been used on Linux; it
builds for macOS, where it has not been run yet.

It is a rumbo plugin, `claude`, which brings the provider `claude`. With
rumbo 0.9.0 or later, which builds a plugin where it is added:

```sh
rumbo plugin add <address of this repository>   # or the path of a copy of it
```

rumbo copies the repository, builds it with Go (`plugin.yaml` says how),
and starts the provider, which is the test that it works. `rumbo plugin
update claude` brings it up to date and builds it again; `rumbo plugin rm
claude` takes it out. Your sign-in is kept apart, and stays.

If you registered it by hand before, as `claude`, the plugin is refused
while that registration is there. Take out the line `command:` under
`providers:`, `claude:` in rumbo's `config.yaml`, and add the plugin: the
model and the options you set for it are kept, and apply to the plugin's.
`rumbo provider rm claude` works too, but takes those with it.

Without plugins, build it and register it yourself:

```sh
# From the rumbo-claude directory:
go build -buildvcs=false -o rumbo-claude .
rumbo provider add claude "$PWD/rumbo-claude"
```

Then:

```sh
rumbo -p claude
```

In rumbo's interface:

1. `/login` gives a link: open it in your browser.
2. Sign in with your Claude account and accept the authorization.
3. Paste the `code#state` code Anthropic shows (the final URL, with `code`
   and `state`, is taken too). It is asked for as a secret.
4. `/use claude` offers the models your account can use.
5. `/account` says whether a sign-in is stored, with the link to your plan's
   usage.
6. `/logout` deletes this provider's tokens on this machine. It does **not**
   revoke them at Anthropic, or sign out other programs.

It signs in with [pi](https://pi.dev)'s **copy code** flow, so the browser
can be on another machine: there is no local listener, and no browser is
opened for you. pi's alternative, an HTTP callback, is not implemented.

## Credentials

- Tokens are kept in `$XDG_CONFIG_HOME/rumbo-claude/claude.json`, or in
  `~/.config/rumbo-claude/claude.json` when that variable is not set, and in
  `~/Library/Application Support/rumbo-claude/claude.json` on macOS.
- The directory is private (`0700`), and so is the file (`0600`), which is
  written atomically.
- A token is renewed five minutes before it expires, and renewed and the
  request retried once when the API rejects it with HTTP 401. A file lock,
  which a cancel interrupts, coordinates the processes, so one reuses a
  renewal another already made.
- Temporary network failures, overload, and the limits of the OAuth endpoint
  are reported to rumbo as retryable. A cancel is never retried.
- When OAuth says which scopes it granted, `user:inference` is checked before
  the tokens are stored; a renewal that leaves the scopes out keeps the
  earlier ones.
- It does not read, import, or change the credentials of pi or Claude Code.
- It does not use `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ant`
  profiles, or an API URL from the environment instead of signing in.

## Models and options

After signing in, `rumbo provider models claude` lists the model IDs, and
`rumbo provider set claude model=<model-id>` chooses the one new sessions
start with.

`effort` takes `low`, `medium`, `high`, `xhigh`, or `max`, as the model
allows. With rumbo 0.6.0 or later, these set it for the provider, show it,
and clear it:

```sh
rumbo provider set claude effort=high
rumbo provider show claude
rumbo provider set claude effort=
```

In the terminal UI, `/session-set effort=low` sets it for one session over
the provider's, and `/session-set effort=` gives the session the provider's
again. When neither sets one, the model uses its own default.

rumbo 0.6.0 does not carry over what 0.5.0 and earlier registered: after
upgrading from those, register the executable again and set its settings
again.

## What it sends and receives

It streams text, tool calls and results, signed reasoning, token usage, and
the context's size when the API reports it. Rate limits and overload are
marked for a retry, also when they come inside the stream after an HTTP 200.
A stream without `message_stop` and a stop reason is not taken for a whole
response, and the HTTP body is always closed. A context too large for the
model is reported to rumbo, which compacts it.

The images of a tool's result, such as an image file rumbo's `read` shows or
a screenshot of its computer tool, go to the model after the result's text,
as image blocks of the result. Those a person gives with a task, or with
what they tell a run, go before their words, as the API advises.

Tools keep their whole JSON Schema, `$defs`, unions, and
`additionalProperties` included. The context's size is asked of the API for
each model, for at most five seconds, until an answer is valid, and that one
is kept: a temporary failure does not leave the size unknown for good.

## Compatibility and warning

It reproduces the public OAuth parameters and the Claude Code compatibility
pi uses: the OAuth client, the scopes, the `claude-cli` and beta headers,
Claude Code's system prefix, and the canonical tool names on the wire. rumbo
gets its tools' own names back. **It adds that prefix to the system prompt
it is sent**: a decision of this external provider, not a change to rumbo's
transcript or to the rules of rumbo's repository.

This is not an official Anthropic integration. Anthropic's terms may not
allow a subscription to be used by a third-party program; that it can be
done does not mean it is allowed. Anthropic may refuse it, change the flow,
or restrict the account. The requests it takes count against the
subscription's limits. Nothing falls back to billing an API key.

It has been used with a real subscription account: long sessions with
`claude-opus-5-5`, with tool calls, signed reasoning, prompt caching, and
compaction, and an image in a tool's result that the model read back
correctly.

## Keeping up with rumbo

`protocol/types.go` and `protocol/serve.go` are copies of rumbo's, unchanged.
rumbo's protocol only grows. When it does, copy both files again from
rumbo's `protocol/`, and send what is new from `internal/claude`, as was done
for the images of a tool's result. With rumbo checked out beside this
directory, these say whether they are still the same:

```sh
diff ../rumbo/protocol/types.go protocol/types.go
diff ../rumbo/protocol/serve.go protocol/serve.go
```

## Tests

```sh
gofmt -l .
go vet ./...
go test -race ./...
go build -buildvcs=false -o rumbo-claude .
```

The tests call no outside service: they run against local HTTP servers, and
the real protocol in a subprocess. They cover PKCE, the OAuth state,
cancelling, shared renewal and rotation, permissions, errors that show no
token, streaming, tools and the images of their results, reasoning, caching,
options, and a real JSON-RPC session.

## Provenance

The Messages adapter, the credential store, and the protocol's server and
types come from rumbo (MIT, `LICENSE`), changed to work on their own. The
OAuth flow and the Claude Code compatibility follow pi's
`packages/ai/src/auth/oauth/anthropic.ts` and
`packages/ai/src/api/anthropic-messages.ts` (MIT, `LICENSE.pi`).
