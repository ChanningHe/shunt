{
  lib,
  buildGoModule,
  version ? "dev",
}:
buildGoModule {
  pname = "shunt";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../main.go
      ../internal
    ];
  };

  vendorHash = "sha256-b1ZBrEFFncP6buCgd+tf81tz4pEvdY/eZ4zZhKkjHMA=";

  ldflags = [
    "-s"
    "-w"
  ];

  # The tests listen on loopback, which the Darwin sandbox denies by default.
  __darwinAllowLocalNetworking = true;

  meta = {
    description = "Local Claude Code router: subagents go to a third-party provider";
    mainProgram = "shunt";
    platforms = lib.platforms.unix;
  };
}
