# shunt

A local passthrough router for Claude Code (Go). The main thread keeps using the
claude.ai subscription; **subagent** requests whose model is `shunt/<route>` are sent
to a third-party Anthropic-compatible API instead, to save subscription quota. Several
routes to different providers can exist side by side and be switched while sessions run.

```
Claude Code ──► shunt (127.0.0.1:27486) ──► api.anthropic.com      everything else, byte-for-byte
                  │                     └─► provider of the route  model "shunt/<route>" + subagent marker
                  └─ /_shunt/  admin API + web UI  ◄── shunt provider|route|tui|web
```

Providers, keys and routes live in two plain files in `~/.config/shunt/`; there is no
database. The CLI, the TUI and the web UI all change them through the same admin API.

## Invariants (guarded by tests)

- **I1** Unrouted requests go to `https://api.anthropic.com` with the body byte-for-byte unchanged and all
  headers except hop-by-hop ones (`Authorization`, `anthropic-beta` incl. `oauth-2025-04-20` kept). Nothing is
  stripped, rewritten or disguised. No `User-Agent`/`Accept-Encoding` is added. (`proxy_test.go`)
- **I2** Third-party requests never contain `sk-ant-` anywhere. Headers are built from an allowlist (inbound
  `authorization`, `x-api-key`, `x-claude-code-*`, `anthropic-beta`, … never pass), the body is re-encoded
  from a top-level allowlist, and the final bytes are scanned before sending; a hit is refused with 502.
  (`internal/router/i2_test.go`, incl. `FuzzI2`)
- Listens on loopback only; proxy requests with a non-local `Host` (DNS rebinding) or any `Origin` header
  (browsers) are refused with 403. Redirects are relayed, never followed; the passthrough target is pinned to
  `https://api.anthropic.com` and is not configurable through files or the API.
- The `shunt/` model namespace belongs to the router: an id without a matching route gets a 400, never a
  passthrough. Likewise nothing under `/_shunt/` is ever forwarded upstream.
- Logs carry method, path, model, target, status and duration — never bodies, keys or the admin token.
  The admin API never returns a key, only its state (`literal`, `file`, `missing`, `invalid`).

## Routing

| Request | Goes to |
|---|---|
| model `shunt/<route>` **and** subagent (`system[0]` billing block has `cc_is_subagent=true` **and** `x-claude-code-agent-id` header) | the route's provider, with the route's upstream model |
| model `shunt/<route>` without the subagent signals (main thread, side queries) | **400** with an explanatory message |
| model `shunt/<anything>` without a route | **400** naming the missing route |
| `shunt/<route>` `count_tokens` | always estimated locally (~4 bytes/token), nothing leaves the machine |
| anything else | Anthropic, unchanged |

A route is a name plus a target: `shunt route set coder deepseek deepseek-flash`. Point agents at it with the
custom id, which Claude Code sends as is (verified with 2.1.322):

- agent frontmatter or `--agents` JSON: `model: shunt/coder`
- every subagent without its own model: `CLAUDE_CODE_SUBAGENT_MODEL=shunt/coder`

Changing a route's provider or model takes effect on the next request; sessions keep running.

**Alias routes.** The Agent tool's `model` parameter only takes `sonnet`, `opus`, `haiku` or `fable`. A route
with one of these names is additionally bound to that alias: `shunt on` and `shunt claude` export
`ANTHROPIC_DEFAULT_<ALIAS>_MODEL=shunt/<alias>`. Use them sparingly:

- Whatever uses that alias outside subagents gets the 400. With a `sonnet` route every session has one side
  query rejected before Claude Code falls back to another model; a route named after the alias your main
  thread runs on (often `fable` or `opus`) breaks the main thread.
- Not routed either way: full ids such as `claude-sonnet-5-5`, fork subagents (they inherit the parent).

## Configuration

```
~/.config/shunt/            $SHUNT_CONFIG_DIR, $XDG_CONFIG_HOME/shunt or --config-dir override it
├── config.toml             providers and routes; no secrets, safe to keep in dotfiles
└── keys.toml               mode 0600; one entry per provider
```

```toml
# config.toml
listen = "127.0.0.1:27486"            # optional

[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"

[routes.coder]
provider = "deepseek"
model = "deepseek-flash"
```

```toml
# keys.toml
deepseek = "sk-..."                               # the key itself, or
kimi     = "file:///run/secrets/kimi-api-key"     # a file holding it (sops-nix, agenix, ...)
```

- Each file may be TOML, YAML (`.yaml`/`.yml`) or JSON; one format per file, new files are TOML. Unknown
  fields are rejected.
- `shunt serve` starts from these files alone. To move to another machine, copy the directory.
- A `file://` key is re-read when the file changes, so a secret that appears after startup or is rotated
  needs no restart. A key that looks like an Anthropic credential disables its provider.
- Hand edits are fine: run `shunt reload` (or `systemctl --user reload shunt`) afterwards. A change made
  through the CLI, TUI or web UI rewrites the affected file in its own format; **comments and key order in
  that file are lost**. Files that did not change are not touched.
- A broken config never takes the router down: it starts (or keeps running) as a plain passthrough, logs the
  error and shows it in `shunt status`; changes through the API are refused until the file is fixed.
- A provider may also set `beta_allow`, `top_keys` and `block_types` to tune sanitizing; these are edited by
  hand only.

## Sanitizing for third parties (`internal/router/sanitize.go`)

Billing block removed from `system`; top-level allowlist (drops `metadata`, `safeguards`, `context_management`,
…); `output_config` keeps only `effort`; `role:"system"` messages are folded: `tool_addition` definitions
hoisted into `tools`, text attached to the nearest user message; server tools (`web_search_*`, `web_fetch_*`,
`advisor_*`, any typed tool) and `defer_loading` tools dropped; tools reduced to name/description/input_schema;
`cache_control` stripped; `redacted_thinking` and unsupported block types dropped; `thinking` mapped to
enabled/disabled (disabled when `tool_choice` forces a tool).

## Install

```sh
go build -o ~/.local/bin/shunt .
# Linux:  deploy/shunt.service     (systemd --user, see header comment; create ~/.config/shunt first)
# macOS:  deploy/local.shunt.plist (launchd, see header comment)

shunt provider add deepseek -b https://api.deepseek.com/anthropic
shunt provider set-key deepseek          # reads the key from stdin, no echo on a terminal
shunt route set coder deepseek deepseek-flash
shunt route test coder                   # one small request through the route: is the key and model right?
shunt on                                 # then restart Claude Code sessions
```

### Nix

The flake has the package (`nix run .#shunt -- --help` in a checkout, `packages.<system>.shunt`,
`overlays.default`) and a per-user service module for each of NixOS, nix-darwin and home-manager:

```nix
{
  inputs.shunt.url = "git+ssh://git@HOST/OWNER/shunt.git";   # wherever this repository lives
  # NixOS:        imports = [ inputs.shunt.nixosModules.default ];
  # nix-darwin:   imports = [ inputs.shunt.darwinModules.default ];
  # home-manager: imports = [ inputs.shunt.homeModules.default ];

  services.shunt.enable = true;      # the service, and shunt on PATH

  # Optional: declare the config instead of using shunt provider/route.
  services.shunt.settings = {
    providers.deepseek.base_url = "https://api.deepseek.com/anthropic";
    routes.coder = { provider = "deepseek"; model = "deepseek-flash"; };
  };
  services.shunt.keyFiles.deepseek = config.sops.secrets.deepseek.path;
}
```

- The service runs as the logged-in user (systemd user unit or launchd agent) on `~/.config/shunt`; on NixOS
  every user gets one. `shunt on` is still yours to run once: the modules never touch `~/.claude`.
- Without `settings` and `keyFiles` the module only runs the service, and the files stay yours.
- `settings` becomes a read-only `config.toml` in the Nix store, so `shunt provider`/`shunt route`, the TUI and
  the web UI refuse changes; `keyFiles` becomes a `keys.toml` of `file://` references. No option takes a key
  itself, so keys never reach the store. A file you wrote by hand is moved to `<name>.pre-nix` first.
- After a rebuild that changes `settings`, restart the service if your setup does not do it for you
  (`systemctl --user restart shunt`).
- Checked: `nix flake check` builds the package and boots a NixOS VM with the module. The nix-darwin and
  home-manager modules are only evaluated, and nothing has run on macOS.

## Usage

```
shunt serve    [--config-dir DIR] [-l, --listen ADDR]      run the router
shunt provider list | add NAME -b URL [--key-file PATH] | rm NAME | set-key NAME [--key-file PATH] | rm-key NAME
shunt route    list | set NAME PROVIDER MODEL | rm NAME | test NAME
shunt reload   [--pid PID]                                 re-read the config files (--pid: only SIGHUP that process)
shunt activity [-n N]                                      the router's most recent requests and where they went
shunt tui                                                  terminal UI
shunt web      [--no-open]                                 web UI, opened with a one-time login link
shunt on | off | status   [-s, --settings FILE] [-f, --force]
shunt claude   [--router-url URL] -- [claude args...]
```

Options are GNU style: `-x` short, `--long` / `--long=value`. Every command takes `-h, --help`; commands that
talk to the router take `--config-dir DIR` and `-u, --url URL`.

**One path to the config.** `provider`, `route`, `reload`, the TUI and the web UI are clients of the admin API
at `http://127.0.0.1:27486/_shunt/api/v1`; the running router is the only writer of the config files. When it
is down these commands say so; edit the files by hand instead, they are read at the next start.

The admin API accepts only local, same-origin requests carrying a bearer token. The token is created at each
start in `$XDG_RUNTIME_DIR/shunt/token-<port>` (mode 0600; the config directory if that variable is unset),
so other local users cannot change where your subagent traffic and keys go. `shunt web` trades it for a
one-time code in the link; the page keeps the token in memory only, so a reload needs `shunt web` again.
No command takes a key as an argument. Changing a provider's host through the API clears its literal key.

**Seeing what happens.** The router keeps the last 200 requests in memory (time, path, model, target, status,
duration; never bodies or headers) and a request count per route. `shunt activity`, the TUI and the web UI
show them, so you can tell at a glance that the main thread went to `anthropic` and a subagent to its
provider; a `rejected` line is a request shunt answered itself (see the routing table). `shunt route test NAME`
(`t` in the TUI, *Test* in the web UI) makes the router send one `max_tokens: 16` request with the route's
provider, key and model and reports the provider's status or error text; it costs a few tokens there. The
TUI and the web UI refresh on their own every few seconds.

**Turning routing on** is separate from the config and local to Claude Code's `settings.json`:

| | `shunt on` (settings mode) | `shunt claude` (env mode) |
|---|---|---|
| How | writes `ANTHROPIC_BASE_URL` (and the alias variables of alias routes) into `settings.json` `env` | starts `claude` with the same variables in its environment |
| Scope | every Claude Code session, incl. `claude --bg` / `claude agents` | only the session it starts (and its subagents, incl. `run_in_background`) |
| Background sessions | ✅ | ❌ refused: they run under a shared daemon that may not see this environment and would silently bypass the router |
| Undo | `shunt off`, restart sessions | exit the session |

`shunt on` refuses while the router is unreachable (with routing on and the router down, every Claude Code
request fails) and refuses to replace values it did not write (a company gateway in `ANTHROPIC_BASE_URL`, a
pinned alias model); `--force` overrides. `shunt off` needs no router and removes only shunt's own values
unless `--force` is given. It keeps the file's key order and 2-space layout (`on` then `off` restores the file
byte-for-byte), writes atomically and through symlinks (`~/.claude/settings.json` may point into a dotfiles
repo). `$CLAUDE_CONFIG_DIR` is honoured.

Adding or removing an **alias** route changes which variables `settings.json` needs. `shunt route` and the TUI
update it in the same step when routing is on; the web UI cannot touch `settings.json` and tells you to run
`shunt on`. Removed variables only disappear from running sessions after a restart. `shunt status` reports
any drift.

`shunt claude` passes everything to `claude` unless a literal `--` is present, in which case options before
it belong to shunt: `shunt claude -c`, `shunt claude --router-url http://127.0.0.1:18081 -- --model opus`.
A `settings.json` `ANTHROPIC_BASE_URL` overrides the environment; shunt warns when one is set.

`shunt status` exits non-zero when routing is on but the router is down.

## Off switch

If the router is down while routing is on, **all** of Claude Code fails. Turn it off with `shunt off` and
restart Claude Code sessions.

Upgrading from a build that routed `ds/…`: run the **old** binary's `shunt off` first (the new one no longer
recognises `ds/…` values as its own), reinstall the unit/plist, move the key from the env file into
`keys.toml`, recreate the route and run `shunt on`.

Upgrading from a build whose default port was 18080: the default is now **27486**. After restarting the
router run `shunt on` again so `settings.json` points at the new port, then restart Claude Code sessions.
Nothing changes if your config sets `listen` itself.

## Terms of Service notes (checked 2026-11-19; risk assessment, not legal advice)

- Gateways in front of a subscription login are a documented configuration: "a saved claude.ai login remains
  the active credential, so its usage limits and billing apply. Gateways that pass this traffic on to Anthropic
  must forward `Authorization` and the OAuth capability in `anthropic-beta`"
  (https://code.claude.com/docs/en/llm-gateway). shunt forwards that traffic unchanged (I1).
- Routing to non-Claude models is *unsupported*, not prohibited: Anthropic "doesn't support routing Claude Code
  to non-Claude models through any gateway" (same page and https://code.claude.com/docs/en/gateways).
- Credentials: "developers may not collect, store, or intermediate Claude.ai credentials or session tokens"
  and "may not … route requests through Free, Pro, Max, or Ultra plan credentials on behalf of their users"
  (https://code.claude.com/docs/en/legal-and-compliance). shunt is a personal tool for the account owner; the
  token is only passed through in memory to Anthropic, never logged, stored or sent to a third party (I2).
- Never strip or alter the attribution block on Anthropic-bound traffic; it is removed only from requests
  that go to the third party.
- The subagent check also reads the undocumented `cc_is_subagent` billing field of our own traffic, next to
  the documented `x-claude-code-agent-id` header; if it changes, routed requests fail loudly (400).
- Re-check these pages after Claude Code updates; their wording changed during 2026.

## Known limitations

- WebSearch/WebFetch server tools are unavailable inside routed subagents.
- JSON Schema regex keywords (`pattern`, `patternProperties`) are removed from tool schemas sent to the
  provider (DeepSeek rejects e.g. `^[^\\0]*$` from the Artifact tool); argument validation stays client-side.
- Claude Code 2.1.322 treats an unknown id such as `shunt/coder` as a 200K context window and compacts earlier
  than needed. Safe, just wasteful.
- A routed subagent whose context legitimately contains `sk-ant-` (e.g. working on this repo's tests) is
  refused with 502 by design (fail closed). Use a full Claude model id for such work.
- With `ANTHROPIC_BASE_URL` set, Remote Control is disabled.
- Code and context of routed subagents (including Claude Code's system prompt and text written by Claude) are
  sent to the route's provider; check its data/training policy and decide per project whether that is acceptable.
- Any local user can reach the loopback port and spend a provider key (the subagent signals can be forged);
  only the admin API is token-protected. Fine on a single-user machine; on shared hosts the proxy port needs
  a unix socket or peer-credential check.
- Whether interactive sessions send background requests on the `haiku` alias is untested; avoid a route
  named `haiku` unless you have checked.
- `shunt claude` only detects `agents` as claude's first argument; `claude --verbose agents` is not refused.

## Development

```
main.go             the binary: hands the command line to internal/cli
internal/router/    request path: passthrough, sanitizing, provider hand-off, route test, serving
internal/admin/     management API, its Go client, request activity, embedded web UI
internal/config/    config.<ext> + keys.<ext>: model, codecs, single-writer store
internal/cli/       commands, option parsing, Claude Code settings.json
internal/tui/       terminal UI
deploy/  e2e/       service units; end-to-end script
flake.nix  nix/     Nix package, NixOS / nix-darwin / home-manager modules, VM test
```

```sh
.claude/check                                          # gofmt, go vet, go test -race
nix flake check                                        # package build + NixOS VM test of the module
go test -fuzz FuzzI2 -fuzztime 1m ./internal/router    # longer I2 fuzzing
DEEPSEEK_API_KEY=... e2e/run.sh                        # real Claude Code + shunt + DeepSeek, cases A–H (configured through the CLI)
```
