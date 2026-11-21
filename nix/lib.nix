# What the three modules share: the launcher script and the systemd unit body.
{
  lib,
  pkgs,
  cfg,
}:
let
  toml = pkgs.formats.toml { };
  shunt = lib.getExe cfg.package;
  coreutils = "${pkgs.coreutils}/bin";

  generated = {
    "config.toml" =
      if cfg.settings == null then null else toml.generate "shunt-config.toml" cfg.settings;
    "keys.toml" =
      if cfg.keyFiles == { } then
        null
      else
        toml.generate "shunt-keys.toml" (lib.mapAttrs (_: path: "file://${path}") cfg.keyFiles);
  };

  # A generated file is linked into the config dir; a file the user wrote is
  # moved aside first, never overwritten. A file that is no longer generated
  # loses its (now stale) store link and goes back to the user.
  place =
    name: file:
    if file == null then
      ''
        case "$(${coreutils}/readlink "$dir/${name}" 2>/dev/null)" in
          ${builtins.storeDir}/*) ${coreutils}/rm -f "$dir/${name}" ;;
        esac
      ''
    else
      ''
        if [ -e "$dir/${name}" ] && [ ! -L "$dir/${name}" ]; then
          if [ -e "$dir/${name}.pre-nix" ]; then
            echo "shunt: $dir/${name} and $dir/${name}.pre-nix both exist; move one away" >&2
            exit 1
          fi
          ${coreutils}/mv "$dir/${name}" "$dir/${name}.pre-nix"
          echo "shunt: moved $dir/${name} to ${name}.pre-nix; the file now comes from the Nix configuration" >&2
        fi
        ${coreutils}/ln -sfn ${file} "$dir/${name}"
      '';
in
rec {
  # launcher { log = null; }: prepares ~/.config/shunt and execs the router.
  # log is a path below $HOME for stderr (launchd has no journal).
  launcher =
    {
      log ? null,
    }:
    pkgs.writeShellScript "shunt-launch" ''
      set -eu
      ${lib.optionalString (log != null) ''
        ${coreutils}/mkdir -p "$(${coreutils}/dirname "$HOME/${log}")"
        exec >>"$HOME/${log}" 2>&1
      ''}
      dir="$HOME/.config/shunt"
      ${coreutils}/mkdir -p -m 700 "$dir"
      ${lib.concatStrings (lib.mapAttrsToList place generated)}
      exec ${shunt} serve --config-dir "$dir"
    '';

  # The [Service] section of deploy/shunt.service. ReadWritePaths needs the
  # directory to exist before the unit starts: see tmpfiles.
  serviceConfig = {
    ExecStart = launcher { };
    ExecReload = "${shunt} reload --pid $MAINPID";
    Restart = "always";
    RestartSec = 1;
    RuntimeDirectory = "shunt";
    RuntimeDirectoryMode = "0700";
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = "read-only";
    ReadWritePaths = "%h/.config/shunt";
    RestrictAddressFamilies = "AF_INET AF_INET6 AF_UNIX";
    LockPersonality = true;
  };

  tmpfiles = [ "d %h/.config/shunt 0700 - - -" ];

  description = "shunt: local Claude Code router (subagents -> third-party provider)";
}
