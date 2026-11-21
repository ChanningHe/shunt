# NixOS VM test of the module: the user unit starts under its sandbox, the
# declared config is served read-only, and no key is in the store.
{ module }:
{
  name = "shunt";

  nodes.machine =
    { ... }:
    {
      imports = [ module ];
      users.users.alice = {
        isNormalUser = true;
        linger = true; # the user manager runs without a login
      };
      services.shunt = {
        enable = true;
        settings = {
          providers.deepseek.base_url = "https://api.deepseek.com/anthropic";
          routes.coder = {
            provider = "deepseek";
            model = "deepseek-flash";
          };
        };
        keyFiles.deepseek = "/run/shunt-test-key";
      };
    };

  # Without settings and keyFiles the module only runs the service.
  nodes.plain =
    { ... }:
    {
      imports = [ module ];
      users.users.alice = {
        isNormalUser = true;
        linger = true;
      };
      services.shunt.enable = true;
    };

  testScript = ''
    def alice(cmd):
        return "su - alice -c 'XDG_RUNTIME_DIR=/run/user/$(id -u alice) " + cmd + "'"

    machine.succeed("echo sk-test-not-a-real-key > /run/shunt-test-key")
    machine.wait_for_unit("user@1000.service")
    machine.wait_until_succeeds(alice("systemctl --user is-active shunt.service"))
    machine.wait_for_open_port(27486)

    with subtest("the declared route is served"):
        out = machine.succeed(alice("shunt route list"))
        assert "coder" in out and "deepseek-flash" in out, out
        machine.succeed(alice("shunt status"))

    with subtest("generated files are read-only links without a key"):
        machine.succeed("readlink /home/alice/.config/shunt/config.toml | grep ^/nix/store/")
        keys = machine.succeed("cat /home/alice/.config/shunt/keys.toml")
        assert "file:///run/shunt-test-key" in keys and "sk-" not in keys, keys
        machine.fail(alice("shunt route set other deepseek some-model"))
        machine.fail(alice("echo sk-x | shunt provider set-key deepseek"))

    with subtest("reload and restart work under the sandbox"):
        machine.succeed(alice("systemctl --user reload shunt.service"))
        machine.succeed(alice("systemctl --user restart shunt.service"))
        machine.wait_until_succeeds(alice("shunt route list"))

    with subtest("a hand-written config is moved aside, not overwritten"):
        machine.succeed(alice("systemctl --user stop shunt.service"))
        machine.succeed(alice("rm ~/.config/shunt/config.toml && echo \\# mine > ~/.config/shunt/config.toml"))
        machine.succeed(alice("systemctl --user start shunt.service"))
        machine.wait_until_succeeds(alice("shunt route list | grep coder"))
        machine.succeed("grep mine /home/alice/.config/shunt/config.toml.pre-nix")
        machine.succeed("readlink /home/alice/.config/shunt/config.toml | grep ^/nix/store/")

    with subtest("without settings the config stays with the user"):
        plain.wait_for_unit("user@1000.service")
        plain.wait_for_open_port(27486)
        plain.succeed(alice("shunt provider add deepseek -b https://api.deepseek.com/anthropic"))
        plain.succeed(alice("shunt route set coder deepseek deepseek-flash"))
        plain.succeed("test -f /home/alice/.config/shunt/config.toml -a ! -L /home/alice/.config/shunt/config.toml")
        plain.succeed(alice("systemctl --user restart shunt.service"))
        plain.wait_until_succeeds(alice("shunt route list | grep coder"))
  '';
}
