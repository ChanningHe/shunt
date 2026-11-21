{
  description = "shunt: local Claude Code router (subagents -> third-party provider)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      version = self.shortRev or self.dirtyShortRev or "dev";
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAll = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      options = import ./nix/options.nix { inherit version; };
    in
    {
      packages = forAll (pkgs: rec {
        shunt = pkgs.callPackage ./nix/package.nix { inherit version; };
        default = shunt;
      });

      overlays.default = final: _prev: {
        shunt = final.callPackage ./nix/package.nix { inherit version; };
      };

      nixosModules.default.imports = [
        options
        ./nix/nixos.nix
      ];
      darwinModules.default.imports = [
        options
        ./nix/darwin.nix
      ];
      homeModules.default.imports = [
        options
        ./nix/home.nix
      ];

      checks = lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (system: {
        nixos = nixpkgs.legacyPackages.${system}.testers.runNixOSTest (
          import ./nix/test.nix { module = self.nixosModules.default; }
        );
      });
    };
}
