# Pinned tools that the acceptance suites need and that nixpkgs does not have
# at the pinned versions.
#
# Solana, Anchor, and Hermes use upstream release binaries patched for Nix.
# SPL Token and tuktuk tools are built from crates.io with the pinned Rust
# toolchain.
{ lib, stdenv, fetchurl, fetchCrate, autoPatchelfHook, rustPlatform
, pkg-config, perl, protobuf, openssl, zlib, udev }:
let
  # Install a prebuilt executable from a pinned release.
  binary = { pname, version, url, hash, bin ? pname }: stdenv.mkDerivation {
    inherit pname version;
    src = fetchurl { inherit url hash; };
    dontUnpack = true;
    nativeBuildInputs = [ autoPatchelfHook ];
    buildInputs = [ stdenv.cc.cc.lib openssl zlib udev ];
    installPhase = ''
      install -D -m 755 "$src" "$out/bin/${bin}"
    '';
  };

  # Build a pinned binary crate from crates.io. spl-token-cli links libudev
  # through hidapi. Its Trezor dependency (rusb) compiles the copy of libusb
  # that libusb1-sys bundles. The linker drops it, because spl-token never
  # calls it.
  crate = { pname, version, hash, cargoHash }: rustPlatform.buildRustPackage {
    inherit pname version cargoHash;
    src = fetchCrate { inherit pname version hash; };
    nativeBuildInputs = [ pkg-config perl protobuf ];
    buildInputs = [ openssl udev ];
    doCheck = false;
  };
in {
  # Agave v4.3.0 release layout, including cargo-build-sbf and its SDK.
  # Use the separately pinned SPL Token CLI instead of the bundled binary.
  solana = stdenv.mkDerivation {
    pname = "solana";
    version = "4.3.0";

    src = fetchurl {
      url = "https://github.com/anza-xyz/agave/releases/download/v4.3.0/solana-release-x86_64-unknown-linux-gnu.tar.bz2";
      hash = "sha256-yXKJqKux0O+0l9i1yyhbqr2bf46mZH9dFFxdyP82Eeg=";
    };

    nativeBuildInputs = [ autoPatchelfHook ];
    buildInputs = [ stdenv.cc.cc.lib openssl zlib udev ];

    # cargo-build-sbf downloads platform tools to ~/.cache/solana on first
    # use. Those tools are not part of this derivation.
    autoPatchelfIgnoreMissingDeps = true;

    installPhase = ''
      mkdir -p "$out"
      cp -r bin "$out/bin"
      rm -f "$out/bin/spl-token"
    '';
  };

  # Anchor releases used by the acceptance suites.
  anchor-0_31_1 = binary {
    pname = "anchor-0.31.1";
    version = "0.31.1";
    url = "https://github.com/coral-xyz/anchor/releases/download/v0.31.1/anchor-0.31.1-x86_64-unknown-linux-gnu";
    hash = "sha256-Xl+PwPdfLD3FzOsIKn9zXQm+IgdUApH/rTcOtbLclZs=";
  };

  anchor-1_0_2 = binary {
    pname = "anchor-1.0.2";
    version = "1.0.2";
    url = "https://github.com/coral-xyz/anchor/releases/download/v1.0.2/anchor-1.0.2-x86_64-unknown-linux-gnu";
    hash = "sha256-UcFYp7ZtttmAKv5ZzXPm14koeREUtVVn3lwHMuiHcOs=";
  };

  # Hermes IBC relayer used by the integration harness.
  hermes = stdenv.mkDerivation {
    pname = "hermes";
    version = "1.13.1";
    src = fetchurl {
      url = "https://github.com/informalsystems/hermes/releases/download/v1.13.1/hermes-v1.13.1-x86_64-unknown-linux-gnu.tar.gz";
      hash = "sha256-7/1rXWmPnj14pOBkpAekSYeslB8/qhBBGjTnJQazqig=";
    };
    sourceRoot = ".";
    nativeBuildInputs = [ autoPatchelfHook ];
    buildInputs = [ stdenv.cc.cc.lib openssl zlib ];
    installPhase = ''
      install -D -m 755 hermes "$out/bin/hermes"
    '';
  };

  # The CLI version that the client suites use. Its ZK ElGamal transcript must
  # match the one that this chain runs.
  spl-token-cli = crate {
    pname = "spl-token-cli";
    version = "5.6.1";
    hash = "sha256-nlbnDTEAUBpY1oz5kqEBpvt0WFA9qmzmokBOftf1qVk=";
    cargoHash = "sha256-fH5AgpMtnOKuB0jf5tiXDAeWBL+JcA7KJ8xjYGd1bek=";
  };

  # CLI and crank-turner versions pinned by the tuktuk acceptance suite.
  tuktuk-cli = crate {
    pname = "tuktuk-cli";
    version = "0.2.18";
    hash = "sha256-DHFHikkxzhst50YwYLTax6B2wpxJ1zCPbV2I1IcYddc=";
    cargoHash = "sha256-fh5C8ATgMVIKEYE1LE0p/gOBpl4kun4n3w95ofTWszU=";
  };

  tuktuk-crank-turner = crate {
    pname = "tuktuk-crank-turner";
    version = "0.2.31";
    hash = "sha256-lQNex4esAhVmHwnXDhq0/g9Xdjy88644VcTOp8lIcAA=";
    cargoHash = "sha256-OzV2J51HFk6kIqWx7BmONCOZ+MTehEZimhMk1v/Kl5I=";
  };
}
