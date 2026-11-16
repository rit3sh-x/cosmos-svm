//! Bakes the engine digest into the binary.
//!
//! src/digest.rs is included, not imported, because a build script cannot
//! depend on the crate it builds. Both copies compile the same source, so the
//! constant and the test that recomputes it cannot disagree.

use std::env;

include!("src/digest.rs");

fn main() {
    let engine = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let root = engine.parent().expect("workspace root").to_path_buf();
    let inputs = inputs(&root).expect("reading the digest inputs");
    for path in &inputs {
        println!("cargo:rerun-if-changed={}", path.display());
    }
    println!("cargo:rerun-if-changed={}", root.join(SOURCE).display());
    let digest = digest(&root).expect("computing the engine digest");
    println!("cargo:rustc-env=SVM_ENGINE_DIGEST={}", hex(&digest));
}