{
  description = "Helmr development and build environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    nixpkgs-unstable.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    nixpkgs-bun.url = "github:NixOS/nixpkgs/09061f748ee21f68a089cd5d91ec1859cd93d0be";
  };

  outputs =
    {
      self,
      nixpkgs,
      nixpkgs-unstable,
      nixpkgs-bun,
    }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      formatter = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        pkgs.nixfmt
      );

      packages = forAllSystems (
        system:
        import ./nix/packages {
          inherit
            self
            system
            nixpkgs
            nixpkgs-unstable
            nixpkgs-bun
            ;
        }
      );

      devShells = forAllSystems (
        system:
        import ./nix/dev-shells.nix {
          inherit
            system
            nixpkgs
            nixpkgs-unstable
            ;
          helmrPackages = self.packages.${system};
        }
      );

      apps = forAllSystems (
        system:
        import ./nix/apps.nix {
          inherit
            system
            nixpkgs
            nixpkgs-unstable
            ;
          helmrPackages = self.packages.${system};
        }
      );

      checks = forAllSystems (
        system:
        import ./nix/checks.nix {
          inherit system nixpkgs;
          helmrPackages = self.packages.${system};
        }
      );

      nixosModules.firecracker-host = import ./nix/modules/nixos/firecracker-host.nix;

      overlays.default = final: prev: {
        helmr = self.packages.${prev.stdenv.hostPlatform.system}.helmr;
      };
    };
}
