# home-manager: a systemd user unit on Linux, a launchd agent on macOS.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.shunt;
  shared = import ./lib.nix { inherit lib pkgs cfg; };
  inherit (pkgs.stdenv.hostPlatform) isLinux isDarwin;
in
{
  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    systemd.user.tmpfiles.rules = lib.mkIf isLinux shared.tmpfiles;
    systemd.user.services.shunt = lib.mkIf isLinux {
      Unit.Description = shared.description;
      Service = shared.serviceConfig;
      Install.WantedBy = [ "default.target" ];
    };

    launchd.agents.shunt = lib.mkIf isDarwin {
      enable = true;
      config = {
        ProgramArguments = [ "${shared.launcher { log = "Library/Logs/shunt.log"; }}" ];
        RunAtLoad = true;
        KeepAlive = true;
        ThrottleInterval = 1;
      };
    };
  };
}
