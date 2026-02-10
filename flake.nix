{
  description = "coder-k8s development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in {
      devShells = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          docsPython = pkgs.python3.withPackages (ps: [
            ps.mkdocs
            ps."mkdocs-material"
            ps."pymdown-extensions"
          ]);
        in {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gnumake
              git

              # Kubernetes dev/demo tools
              kubectl
              kind
              k9s

              goreleaser
              actionlint
              zizmor
              golangci-lint
              govulncheck

              docsPython
            ];
          };
        }
      );
    };
}
