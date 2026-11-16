// The engine digest: a SHA-256 over every build input that can change what the
// engine does.
//
// This file is compiled twice. `build.rs` includes it to compute the digest,
// and bakes the result in as an environment variable. The crate compiles it as
// a module, so a test can recompute the digest from the tree and compare. Both
// copies are the same source, so the algorithm cannot drift from the one that
// produced the constant. The file has no inner attributes and no imports beyond
// these, because an included file shares the build script's namespace.

use std::io;
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

/// The files the digest covers, relative to the workspace root, in the order
/// they are hashed.
///
/// The release profile, the `[patch]` table and the feature selection are all
/// inside these manifests. `Cargo.lock` is the full resolved dependency graph.
/// The digest walks the engine source tree, so a new file is covered with no
/// change to this list. A new build input must be added here in the same
/// change. A digest that covers less than the inputs that can change execution
/// gives confidence, not detection.
pub const FILES: [&str; 4] = ["Cargo.lock", "Cargo.toml", "engine/Cargo.toml", "rust-toolchain.toml"];

/// The engine source tree, relative to the workspace root.
pub const SOURCE: &str = "engine/src";

/// inputs is every file the digest covers, in a deterministic order: the fixed
/// list above, then the source tree sorted by path.
pub fn inputs(root: &Path) -> io::Result<Vec<PathBuf>> {
    let mut out: Vec<PathBuf> = FILES.iter().map(|f| root.join(f)).collect();
    let mut source = Vec::new();
    walk(&root.join(SOURCE), &mut source)?;
    source.sort();
    out.extend(source);
    Ok(out)
}

fn walk(dir: &Path, out: &mut Vec<PathBuf>) -> io::Result<()> {
    for entry in std::fs::read_dir(dir)? {
        let path = entry?.path();
        if path.is_dir() {
            walk(&path, out)?;
        } else {
            out.push(path);
        }
    }
    Ok(())
}

/// digest hashes each input as its path relative to the root, then its length,
/// then its bytes. The path makes a rename a change. The length stops bytes
/// from moving across the boundary between two files.
pub fn digest(root: &Path) -> io::Result<[u8; 32]> {
    let mut hasher = Sha256::new();
    for path in inputs(root)? {
        let relative = path.strip_prefix(root).unwrap_or(&path);
        let name = relative.to_string_lossy().replace('\\', "/");
        let bytes = std::fs::read(&path)?;
        hasher.update((name.len() as u64).to_le_bytes());
        hasher.update(name.as_bytes());
        hasher.update((bytes.len() as u64).to_le_bytes());
        hasher.update(&bytes);
    }
    Ok(hasher.finalize().into())
}

/// hex is the lower-case form the genesis file and the logs carry.
pub fn hex(digest: &[u8; 32]) -> String {
    let mut s = String::with_capacity(64);
    for b in digest {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    fn root() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).parent().unwrap().to_path_buf()
    }

    /// The compiled-in constant is a hash of the tree as it stands, so a change
    /// to any line in the engine, a manifest, Cargo.lock or the toolchain pin
    /// changes it. This test fails if build.rs stops rerunning.
    #[test]
    fn the_compiled_digest_is_the_digest_of_the_tree() {
        assert_eq!(hex(&digest(&root()).unwrap()), crate::ENGINE_DIGEST);
    }

    #[test]
    fn changing_one_input_changes_the_digest() {
        let root = root();
        let before = digest(&root).unwrap();
        let copy = std::env::temp_dir().join(format!("svm-digest-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&copy);
        for path in inputs(&root).unwrap() {
            let target = copy.join(path.strip_prefix(&root).unwrap());
            std::fs::create_dir_all(target.parent().unwrap()).unwrap();
            std::fs::copy(&path, &target).unwrap();
        }
        assert_eq!(digest(&copy).unwrap(), before, "a faithful copy digests the same");

        for file in ["Cargo.lock", "engine/Cargo.toml", "rust-toolchain.toml", "engine/src/lib.rs"] {
            let path = copy.join(file);
            let original = std::fs::read(&path).unwrap();
            let mut edited = original.clone();
            edited.push(b'\n');
            std::fs::write(&path, &edited).unwrap();
            assert_ne!(digest(&copy).unwrap(), before, "{file} is outside the digest");
            std::fs::write(&path, &original).unwrap();
        }
        // A file added to the source tree is covered without listing it.
        std::fs::write(copy.join("engine/src/zz_new.rs"), b"// added").unwrap();
        assert_ne!(digest(&copy).unwrap(), before, "a new source file is outside the digest");
        std::fs::remove_dir_all(&copy).unwrap();
    }
}
