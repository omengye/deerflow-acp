//! Shared identity and portable storage for DeerFlow Desktop.

use std::path::{Path, PathBuf};
use std::sync::OnceLock;

#[cfg(debug_assertions)]
pub const APP_NAME: &str = "DeerFlow Desktop Debug";
#[cfg(not(debug_assertions))]
pub const APP_NAME: &str = "DeerFlow Desktop";

#[cfg(debug_assertions)]
pub const APP_ID: &str = "app.deerflow.desktop.dev";
#[cfg(not(debug_assertions))]
pub const APP_ID: &str = "app.deerflow.desktop";

pub const DATA_DIRECTORY_NAME: &str = "DeerFlow Desktop";

/// The package root, shared by the desktop, its daemon, and the ACP runtime.
/// Set DEER_FLOW_PORTABLE_ROOT before launch to use another portable profile.
/// Development has its own root under the checkout and never reads ~/.waku.
pub fn portable_root() -> PathBuf {
    static ROOT: OnceLock<PathBuf> = OnceLock::new();
    ROOT.get_or_init(|| {
        let checkout = Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .and_then(Path::parent)
            .expect("protocol crate belongs to the desktop workspace");
        resolve_portable_root(
            std::env::var_os("DEER_FLOW_PORTABLE_ROOT")
                .filter(|value| !value.is_empty())
                .map(PathBuf::from)
                .as_deref(),
            std::env::current_exe().ok().as_deref(),
            &checkout.join("temp"),
            &std::env::current_dir().unwrap_or_else(|_| checkout.to_owned()),
            cfg!(debug_assertions),
        )
    })
    .clone()
}

pub fn desktop_data_directory() -> PathBuf {
    portable_root().join("user-data").join("desktop")
}

/// Only called by provider discovery workers; file probes never run in render.
pub fn bundled_acp_binary() -> Option<PathBuf> {
    let binary = if cfg!(windows) {
        "deerflow-acp.exe"
    } else {
        "deerflow-acp"
    };
    let mut candidates = vec![portable_root().join(binary)];
    if cfg!(debug_assertions) {
        let repository = Path::new(env!("CARGO_MANIFEST_DIR")).ancestors().nth(3)?;
        candidates.extend([
            repository.join("dist/portable/DeerFlow").join(binary),
            repository.join("bridge/target/debug").join(binary),
            repository.join("bridge/target/release").join(binary),
        ]);
    }
    candidates.into_iter().find(|candidate| candidate.is_file())
}

/// Never initialize an upstream Waku updater, even with WAKU_FORCE_UPDATER.
pub fn upstream_updates_enabled() -> bool {
    false
}

fn resolve_portable_root(
    override_root: Option<&Path>,
    executable: Option<&Path>,
    development_root: &Path,
    current_directory: &Path,
    development: bool,
) -> PathBuf {
    if let Some(root) = override_root {
        return if root.is_absolute() {
            root.to_owned()
        } else {
            current_directory.join(root)
        };
    }
    if development {
        return development_root.to_owned();
    }
    executable
        .and_then(Path::parent)
        .unwrap_or(current_directory)
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn profile_override_wins_for_both_build_profiles() {
        let cwd = std::env::current_dir().unwrap();
        let override_root = cwd.join("profiles/中文 profile");
        for development in [true, false] {
            assert_eq!(
                resolve_portable_root(
                    Some(&override_root),
                    None,
                    Path::new("temp"),
                    &cwd,
                    development
                ),
                override_root
            );
            assert_eq!(
                resolve_portable_root(
                    Some(Path::new("relative-profile")),
                    None,
                    Path::new("temp"),
                    &cwd,
                    development
                ),
                cwd.join("relative-profile")
            );
        }
    }

    #[test]
    fn packaged_root_follows_executable_while_development_is_isolated() {
        let package = Path::new("package with spaces");
        let executable = package.join("deerflow-desktop.exe");
        let development_root = Path::new("checkout/desktop-app/temp");
        assert_eq!(
            resolve_portable_root(
                None,
                Some(&executable),
                development_root,
                Path::new("other"),
                false
            ),
            package
        );
        assert_eq!(
            resolve_portable_root(
                None,
                Some(&executable),
                development_root,
                Path::new("other"),
                true
            ),
            development_root
        );
    }
}
