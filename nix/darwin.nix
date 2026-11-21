# nix-darwin: a launchd user agent.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.shunt;
  shared = import ./lib.nix { inherit lib pkgs cfg; };
in
{
  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];
    launchd.user.agents.shunt.serviceConfig = {
      ProgramArguments = [ "${shared.launcher { log = "Library/Logs/shunt.log"; }}" ];
      RunAtLoad = true;
      KeepAlive = true;
      ThrottleInterval = 1;
    };
  };
}
