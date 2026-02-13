{
  description = "coder-k8s development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [ "terraform" ];
          };
          docsPython = pkgs.python3.withPackages (ps: [
            ps.mkdocs
            ps."mkdocs-material"
            ps."pymdown-extensions"
          ]);
          ktx = pkgs.writeShellScriptBin "ktx" ''
            exec ${pkgs.kubectx}/bin/kubectx "$@"
          '';
          kns = pkgs.writeShellScriptBin "kns" ''
            exec ${pkgs.kubectx}/bin/kubens "$@"
          '';
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gnumake
              git
              lazygit

              # Kubernetes dev/demo tools
              kubectl
              kind
              k9s
              kubectx
              ktx
              kns

              # Cloud tooling
              awscli2
              terraform

              goreleaser
              actionlint
              zizmor
              golangci-lint
              govulncheck

              docsPython
              yazi
            ];

            shellHook = ''
              alias lg='lazygit'
            '';
          };
        }
      );

      formatter = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        pkgs.nixfmt
      );
    };
}
