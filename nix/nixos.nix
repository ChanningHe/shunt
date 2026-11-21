# NixOS: a systemd user unit, started for every user that logs in.
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
    systemd.user.tmpfiles.rules = shared.tmpfiles;
    systemd.user.services.shunt = {
      inherit (shared) description serviceConfig;
      wantedBy = [ "default.target" ];
    };
  };
}
