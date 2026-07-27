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
            # Node 22, not 20. CI pins node-version: 20 and GitHub still ships
            # 20.x binaries, but Node 20 went end-of-life in April 2026 and the
            # nixpkgs revision locked here (2026-05-29) postdates that, so
            # nodejs_20 evaluates as insecure and `nix develop` refuses to build.
            # 22 satisfies the locked Vite's ^20.19.0 || >=22.12.0 floor.
            pkgs.nodejs_22
            # tmux is a runtime prerequisite, not a convenience: the daemon execs
            # it for every session, so a shell without it cannot run AO.
            pkgs.tmux
            pkgs.git
            # gh is not optional: the daemon shells out to it for pull request,
            # CI, and review facts, and ao doctor checks for a valid token.
            pkgs.gh
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
