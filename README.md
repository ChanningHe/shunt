# shunt

A local router for Claude Code. Your main thread keeps using your Claude subscription;
**subagents** you point at `shunt/<route>` go to a third-party Anthropic-compatible API
(DeepSeek, Kimi, ...) instead, which saves subscription quota.

```
Claude Code ──► shunt (127.0.0.1:27486) ──► api.anthropic.com      everything else, unchanged
                                        └─► your provider          subagents with model "shunt/<route>"
```

- Requests to Anthropic are forwarded byte-for-byte; nothing is rewritten.
- Requests to a provider never carry your Anthropic credentials, and provider keys never go to Anthropic.
- Routes can be added, changed and tested while sessions run.

## Quick start

```sh
go build -o ~/.local/bin/shunt .         # or Homebrew or the Nix flake, see below
shunt serve &                            # or run it as a service, see below

shunt provider add deepseek -b https://api.deepseek.com/anthropic
shunt provider set-key deepseek          # reads the key from stdin, never from an argument
shunt route set coder deepseek deepseek-flash
shunt route test coder                   # one tiny request: are the key and model right?
shunt on                                 # point Claude Code at shunt, then restart its sessions
```

Then give a subagent the model `shunt/coder`:

- one agent: `model: shunt/coder` in its frontmatter (or `--agents` JSON)
- every subagent without its own model: `CLAUDE_CODE_SUBAGENT_MODEL=shunt/coder`

To run it as a service, see the header of `deploy/shunt.service` (systemd user unit) or
`deploy/local.shunt.plist` (launchd), or use Homebrew:

```sh
brew tap channinghe/shunt https://github.com/ChanningHe/shunt
brew install shunt                       # --HEAD for the main branch
brew services start shunt                # instead of shunt serve &
```

The Homebrew service does not see `XDG_CONFIG_HOME` or `SHUNT_CONFIG_DIR` from your shell; it uses
`~/.config/shunt`.

**Off switch.** With routing on and the router down, all of Claude Code fails. Run `shunt off` and restart
your sessions; `shunt off` works without the router.

## Commands

```
shunt provider list | add NAME -b URL | rm NAME | set-key NAME | rm-key NAME
shunt route    list | set NAME PROVIDER MODEL | rm NAME | test NAME
shunt activity          recent requests and where each one went
shunt tui               terminal UI
shunt web               web UI, opened with a one-time login link
shunt on | off | status turn routing on or off in Claude Code's settings.json; show the state
shunt claude -- [args]  start one claude session with routing on, without touching settings.json
shunt reload            re-read the config files after a hand edit
shunt serve             run the router
```

Every command takes `--help`. The TUI and the web UI do the same things as `provider` and `route`. Both show
where each route goes: the web UI as a diagram (routes on the left, shunt in the middle, provider and model
on the right), the TUI as one `shunt/<route> ──▶ provider · model` line per route. A route that carried a
request in the last ten seconds is marked live. Both also list recent requests: `anthropic` for the main
thread, the provider name for a routed subagent, `rejected` for a request shunt refused.

## What gets routed

| Request | Goes to |
|---|---|
| a subagent with model `shunt/<route>` | the route's provider and model |
| model `shunt/<route>` from the main thread, or a route that does not exist | refused with a 400 that says why |
| anything else | Anthropic, unchanged |

A route named `sonnet`, `opus`, `haiku` or `fable` also takes over that alias, so the Agent tool's `model`
parameter can select it. Prefer other names: everything else that uses the alias is refused, and a route
named after the model your main thread runs on breaks the main thread.

## Configuration

Two plain files in `~/.config/shunt/` (`--config-dir` or `$SHUNT_CONFIG_DIR` to change); no database.

```toml
# config.toml: no secrets, safe to keep in dotfiles
listen = "127.0.0.1:27486"            # optional

[providers.deepseek]
base_url = "https://api.deepseek.com/anthropic"

[routes.coder]
provider = "deepseek"
model = "deepseek-flash"
```

```toml
# keys.toml: mode 0600
deepseek = "sk-..."                               # the key itself, or
kimi     = "file:///run/secrets/kimi-api-key"     # a file holding it (sops-nix, agenix, ...)
```

- The files may also be YAML or JSON.
- After a hand edit run `shunt reload`. Changes made through the commands or UIs rewrite the file and drop
  its comments.
- A broken config does not take the router down: it keeps passing everything to Anthropic and `shunt status`
  shows the error.

## Nix

The flake provides the package (`packages.<system>.shunt`, `overlays.default`) and a per-user service module
for NixOS, nix-darwin and home-manager:

```nix
{
  # imports = [ inputs.shunt.nixosModules.default ];   # or darwinModules / homeModules
  services.shunt.enable = true;      # the service, and shunt on PATH

  # Optional: declare the config instead of using shunt provider/route.
  services.shunt.settings = {
    providers.deepseek.base_url = "https://api.deepseek.com/anthropic";
    routes.coder = { provider = "deepseek"; model = "deepseek-flash"; };
  };
  services.shunt.keyFiles.deepseek = config.sops.secrets.deepseek.path;
}
```

- You still run `shunt on` once; the modules never touch `~/.claude`.
- With `settings` the config is read-only, so the commands and UIs refuse to change it. `keyFiles` takes
  paths to key files; keys themselves never enter the Nix store.
- After a rebuild that changes `settings`, run `systemctl --user restart shunt` if the service was not
  restarted for you.
- The NixOS module is tested in a VM. The nix-darwin and home-manager modules are only evaluated, and the
  Homebrew formula is only tested with Homebrew on Linux; nothing has been run on macOS.

## Good to know

- **Your code goes to the provider.** Routed subagents send their context, including Claude Code's system
  prompt, to the route's provider. Check its data policy.
- **Unsupported by Anthropic.** Putting a gateway in front of Claude Code is documented; routing to
  non-Claude models through one is not supported, and may break with any Claude Code update. Not legal advice.
- **Single-user machines.** shunt listens on loopback only and the admin API needs a local token, but any
  local user can reach the proxy port and spend a provider key.
- WebSearch and WebFetch do not work inside routed subagents.
- Claude Code assumes a 200K context window for `shunt/...` models and compacts earlier than needed.
- A routed request whose content contains `sk-ant-` is refused on purpose.
- Remote Control is disabled while routing is on.
- The web UI keeps its login in memory only: reloading the page signs you out, run `shunt web` again.

## Development

```sh
.claude/check                                          # gofmt, go vet, go test -race
nix flake check                                        # package build + NixOS VM test
go test -fuzz FuzzI2 -fuzztime 1m ./internal/router    # fuzz the credential guard
DEEPSEEK_API_KEY=... e2e/run.sh                        # real Claude Code + shunt + DeepSeek
just release 1.2.3                                     # tag v1.2.3, point the formula at it; then push
```
