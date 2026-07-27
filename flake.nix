{
  description = "agent-orchestrator development shell";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        go = pkgs.go_1_25;
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = [
            go
            pkgs.gotools
            # Node 20 is the baseline the frontend, release, and packaging
            # workflows build against. Newer runtimes work locally; match CI here.
            pkgs.nodejs_20
            # tmux is a runtime prerequisite, not a convenience: the daemon execs
            # it for every session, so a shell without it cannot run AO.
            pkgs.tmux
            pkgs.git
          ];

          shellHook = ''
            export GOROOT="${go}/share/go"
            export GOPATH="$PWD/.go"
            export GOBIN="$GOPATH/bin"
            export PATH="$GOBIN:$PATH"
          '';
        };
      }
    );
}
