{
  # Pinned development environment for local builds, tests, and acceptance
  # runs. Enter with `nix develop`, run `make setup` once, then `make test`.
  # `nix develop .#ci` enters CI's shell: build, test, and lint tools only.
  # `nix flake check` builds every packaged tool.
  #
  # Supported platform: x86_64-linux. The validators and pinned Solana
  # and Anchor releases target x86-64 Linux.
  description = "cosmos-svm: the Solana VM on the Cosmos SDK, with every tool its build and acceptance runs need";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    rust-overlay = {
      url = "github:oxalica/rust-overlay";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, rust-overlay }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        overlays = [ rust-overlay.overlays.default ];
      };

      # Match the compiler pinned by rust-toolchain.toml for Agave v4.3.0.
      # The engine digest includes this toolchain version.
      rust = pkgs.rust-bin.stable."1.97.1".default.override {
        extensions = [ "rust-src" "rustfmt" "clippy" "rust-analyzer" ];
      };
      rustPlatform = pkgs.makeRustPlatform { cargo = rust; rustc = rust; };

      # Keep only derivations in the packages and checks outputs.
      tools = pkgs.lib.filterAttrs (_: pkgs.lib.isDerivation)
        (pkgs.callPackage ./nix/tools.nix { inherit rustPlatform; });

      # CI's tools (.github/workflows/ci.yml): go.mod's Go, rust-toolchain.toml's
      # Rust, a C toolchain, make, the perl that openssl-src needs, the linters,
      # and the small tools the scripts call. devShells.ci has this list only.
      # The default shell adds the acceptance tools, which CI does not run.
      # Building three of them from source at once filled a GitHub runner's disk.
      ci = with pkgs; [
        go_1_27
        rust
        gnumake
        perl
        pkg-config
        openssl
        protobuf
        shellcheck
        python3
        git
        curl
        jq
        bzip2
      ];
    in {
      packages.${system} = tools;
      checks.${system} = tools;

      devShells.${system} = {
        ci = pkgs.mkShell {
          packages = ci;
          env.GOTOOLCHAIN = "local";
        };

        default = pkgs.mkShell {
          # rustup provides the proxies used by cargo-build-sbf. The shell
          # hook registers the flake's Rust toolchain with those proxies.
          # CI's tools follow, then the acceptance tools.
          packages = [ pkgs.rustup ] ++ ci ++ (with pkgs; [
            # Hyperlane's agents need these. mold links them, bindgen uses libclang
            # for their RocksDB, and the Sealevel client links libudev through
            # hidapi for hardware wallets.
            mold
            udev
            llvmPackages.libclang.lib

            # Solana, Anchor, SPL, and IBC tooling for acceptance tests.
            tools.solana
            tools.anchor-0_31_1
            tools.anchor-1_0_2
            tools.spl-token-cli
            tools.tuktuk-cli
            tools.tuktuk-crank-turner
            tools.hermes
            nodejs_22

            # JavaScript tooling for the Explorer and third-party suites.
            # Put pnpm before corepack to avoid corepack's pnpm shim.
            pnpm_10
            corepack
            playwright-driver.browsers

            # The settlement run's Postgres needs the docker command.
            docker-client
          ]);

          env = {
            # Use the flake's Go. Do not download a newer toolchain.
            GOTOOLCHAIN = "local";

            # Allow corepack to fetch pinned Yarn versions without prompting.
            COREPACK_ENABLE_DOWNLOAD_PROMPT = "0";

            # Use the bundled browser and skip host-library validation.
            PLAYWRIGHT_BROWSERS_PATH = "${pkgs.playwright-driver.browsers}";
            PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS = "true";

            # bindgen needs it to build Hyperlane's RocksDB dependency.
            LIBCLANG_PATH = "${pkgs.llvmPackages.libclang.lib}/lib";
          };

          # Register the flake's Rust under the name rust-toolchain.toml expects,
          # for cargo-build-sbf's rustup proxies. Keep a rustup-installed copy of
          # the same release; replace a link from an older flake.lock. Set the
          # default toolchain only if none is set.
          shellHook = ''
            t=''${RUSTUP_HOME:-$HOME/.rustup}/toolchains/1.97.1-x86_64-unknown-linux-gnu
            if [ -L "$t" ] || [ ! -e "$t" ]; then mkdir -p "''${t%/*}"; ln -sfn ${rust} "$t"; fi
            unset t
            rustup default >/dev/null 2>&1 || rustup default 1.97.1-x86_64-unknown-linux-gnu >/dev/null
          '';
        };
      };
    };
}
