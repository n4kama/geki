{
  description = "Geki - Discord bot";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["aarch64-linux" "x86_64-linux" "aarch64-darwin"];
    forAllSystems = nixpkgs.lib.genAttrs systems;
  in {
    packages = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in rec {
      geki = pkgs.buildGoModule {
        pname = "geki";
        version = self.shortRev or self.dirtyShortRev or "dirty";
        src = ./.;
        vendorHash = "sha256-NrkJF6hpFTflnowOUR7SR5kRF4OfbwKsxAsOtLqwRsg=";

        # Mirrors what the justfile built: a static, trimmed binary.
        env.CGO_ENABLED = 0;
        ldflags = ["-s" "-w"];

        # Run the test suite at build time so a broken build fails early.
        doCheck = true;

        meta.mainProgram = "geki";
      };
      default = geki;
    });

    devShells = forAllSystems (system: {
      default = nixpkgs.legacyPackages.${system}.mkShell {
        packages = [nixpkgs.legacyPackages.${system}.go];
      };
    });
  };
}
