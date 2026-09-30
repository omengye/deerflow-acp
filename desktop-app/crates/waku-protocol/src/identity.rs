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
/// Unpackaged development has its own root under the checkout and never reads ~/.waku.
pub fn portable_root() -> PathBuf {
    static ROOT: OnceLock<PathBuf> = OnceLock::new();
    ROOT.get_or_init(|| {
        let checkout = Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .and_then(Path::parent)
            .expect("protocol crate belongs to the desktop workspace");
        let executable = std::env::current_exe().ok();
        resolve_portable_root(
            std::env::var_os("DEER_FLOW_PORTABLE_ROOT")
                .filter(|value| !value.is_empty())
                .map(PathBuf::from)
                .as_deref(),
            executable.as_deref(),
            &checkout.join("temp"),
            &std::env::current_dir().unwrap_or_else(|_| checkout.to_owned()),
            cfg!(debug_assertions) && !has_bundled_go_runtime(executable.as_deref()),
        )
    })
    .clone()
}

fn has_bundled_go_runtime(executable: Option<&Path>) -> bool {
    let Some(directory) = executable.and_then(Path::parent) else {
        return false;
    };
    let (config, daemon) = if cfg!(windows) {
        ("deerflow-config-go.exe", "deerflow-acpd.exe")
    } else {
        ("deerflow-config-go", "deerflow-acpd")
    };
    directory.join(config).is_file() && directory.join(daemon).is_file()
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

    #[test]
    fn debug_go_package_uses_executable_directory() {
        let package =
            std::env::temp_dir().join(format!("deerflow-debug-package-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&package).unwrap();
        let desktop = package.join(if cfg!(windows) {
            "deerflow-desktop.exe"
        } else {
            "deerflow-desktop"
        });
        let config = package.join(if cfg!(windows) {
            "deerflow-config-go.exe"
        } else {
            "deerflow-config-go"
        });
        let daemon = package.join(if cfg!(windows) {
            "deerflow-acpd.exe"
        } else {
            "deerflow-acpd"
        });
        assert!(!has_bundled_go_runtime(Some(&desktop)));
        std::fs::write(&config, b"fixture").unwrap();
        std::fs::write(&daemon, b"fixture").unwrap();
        assert!(has_bundled_go_runtime(Some(&desktop)));
        assert_eq!(
            resolve_portable_root(
                None,
                Some(&desktop),
                Path::new("checkout/temp"),
                Path::new("other"),
                !has_bundled_go_runtime(Some(&desktop))
            ),
            package
        );
        std::fs::remove_dir_all(package).unwrap();
    }
}
