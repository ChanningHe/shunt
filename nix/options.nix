# services.shunt.* options, shared by the NixOS, nix-darwin and home-manager
# modules. Everything is per user: the router serves one user's Claude Code
# and keeps its files in ~/.config/shunt.
{ version }:
{
  lib,
  pkgs,
  ...
}:
let
  toml = pkgs.formats.toml { };
in
{
  _file = ./options.nix;

  options.services.shunt = {
    enable = lib.mkEnableOption "the shunt router as a per-user service, with the shunt command on PATH";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { inherit version; };
      defaultText = lib.literalExpression "the shunt package of this flake";
      description = "The shunt package to run.";
    };

    settings = lib.mkOption {
      type = lib.types.nullOr toml.type;
      default = null;
      example = lib.literalExpression ''
        {
          providers.deepseek.base_url = "https://api.deepseek.com/anthropic";
          routes.coder = {
            provider = "deepseek";
            model = "deepseek-flash";
          };
        }
      '';
      description = ''
        Contents of config.toml (listen, providers, routes). When set, the
        file lives in the Nix store and is linked into ~/.config/shunt, so
        `shunt provider`/`shunt route`, the TUI and the web UI refuse to
        change it. When null, the module leaves config.toml alone and those
        commands manage it.
      '';
    };

    keyFiles = lib.mkOption {
      # Not types.path: a path literal would be copied into the world-readable store.
      type = lib.types.attrsOf (lib.types.strMatching "/.*");
      default = { };
      example = lib.literalExpression "{ deepseek = config.sops.secrets.deepseek.path; }";
      description = ''
        Provider name to the absolute path of a file holding its key (sops-nix,
        agenix, ...). When non-empty, keys.toml is generated with file://
        references only and `shunt provider set-key` is refused. When empty,
        the module leaves keys.toml alone. Keys themselves never enter the
        Nix store: there is no option that takes one.
      '';
    };
  };
}
