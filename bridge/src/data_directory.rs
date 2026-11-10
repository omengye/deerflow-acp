use serde::Deserialize;
use std::{
    fs,
    io::Read,
    path::{Component, Path, PathBuf},
};

use crate::{Result, normalize_path};

pub(crate) fn user_data(
    config: Option<&Path>,
    portable: Option<&Path>,
    executable: &Path,
) -> Result<Option<PathBuf>> {
    if let Some(config) = config {
        let parent = config
            .parent()
            .ok_or("configuration has no parent directory")?;
        if parent
            .file_name()
            .is_some_and(|name| name.eq_ignore_ascii_case("config"))
            && parent
                .parent()
                .and_then(Path::file_name)
                .is_some_and(|name| name.eq_ignore_ascii_case("user-data"))
        {
            let data = parent.parent().unwrap();
            if let Some(portable) = portable {
                if normalize_path(data)? != normalize_path(&portable.join("user-data"))? {
                    return Err(
                        "portable root and configuration select different user-data directories"
                            .into(),
                    );
                }
            }
            return Ok(Some(data.to_path_buf()));
        }
        if portable.is_some() {
            return Err("configuration is outside the selected portable profile".into());
        }
        return Ok(None);
    }
    if let Some(portable) = portable {
        return Ok(Some(portable.join("user-data")));
    }
    Ok(crate::portable_root_for(executable).map(|root| root.join("user-data")))
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Binding {
    data_dir: PathBuf,
}

pub(crate) fn resolve(user_data: &Path, requested: Option<&Path>) -> Result<PathBuf> {
    let path = user_data.join("config/runtime-data.json");
    let selected = match fs::File::open(&path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            if user_data
                .join("config/go-harness-state/harness.db")
                .is_file()
                || user_data.join("data/go-harness/harness.db").is_file()
            {
                return Err("runtime-data.json is missing while a historical database exists; refusing to choose another store".into());
            }
            user_data.join("config/go-harness-state")
        }
        Err(error) => return Err(error.into()),
        Ok(file) => {
            let mut bytes = Vec::new();
            file.take(8193).read_to_end(&mut bytes)?;
            if bytes.len() > 8192 {
                return Err("runtime-data.json exceeds 8192 bytes".into());
            }
            let binding: Binding = serde_json::from_slice(&bytes)?;
            if binding.data_dir.as_os_str().is_empty()
                || binding
                    .data_dir
                    .components()
                    .any(|part| !matches!(part, Component::Normal(_)))
            {
                return Err("bound data directory must be relative to user-data".into());
            }
            let selected = user_data.join(binding.data_dir);
            if !selected.join("harness.db").is_file() {
                return Err("bound database is missing; refusing to open another store".into());
            }
            if !fs::canonicalize(&selected)?.starts_with(fs::canonicalize(user_data)?) {
                return Err("bound data directory escapes user-data".into());
            }
            selected
        }
    };
    for relative in ["config/go-harness-state", "data/go-harness"] {
        let other = user_data.join(relative);
        if other.join("harness.db").is_file() && normalize_path(&other)? != normalize_path(&selected)? {
            return Err("a second runtime database exists; archive the inactive store outside runtime paths before starting this profile, then use the archive for skill recovery".into());
        }
    }
    if let Some(requested) = requested {
        if normalize_path(requested)? != normalize_path(&selected)? {
            return Err(format!("data directory override conflicts with portable binding: requested {}, selected {}", requested.display(), selected.display()).into());
        }
    }
    Ok(selected)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn binding_prevents_override_and_missing_binding_never_selects_another_store() {
        let root = tempfile::tempdir().unwrap();
        let data = root.path().join("user-data");
        let old = data.join("config/go-harness-state");
        assert_eq!(resolve(&data, None).unwrap(), old);
        fs::create_dir_all(&old).unwrap();
        fs::write(old.join("harness.db"), b"original history").unwrap();
        assert!(resolve(&data, None).is_err());
        fs::write(
            data.join("config/runtime-data.json"),
            br#"{"data_dir":"config/go-harness-state"}"#,
        )
        .unwrap();
        assert_eq!(resolve(&data, None).unwrap(), old);
        assert!(resolve(&data, Some(&old)).is_ok());
        assert!(resolve(&data, Some(&data.join("data/go-harness"))).is_err());
        fs::remove_file(old.join("harness.db")).unwrap();
        assert!(resolve(&data, None).is_err());
        assert!(!data.join("data/go-harness").exists());
    }

    #[test]
    fn configuration_selects_its_profile_and_rejects_inherited_other_profile() {
        let root = tempfile::tempdir().unwrap();
        let config = root.path().join("user-data/config/config.yaml");
        let executable = root.path().join("elsewhere/deerflow-acp");
        assert_eq!(
            user_data(Some(&config), None, &executable).unwrap(),
            Some(root.path().join("user-data"))
        );
        assert!(user_data(Some(&config), Some(&root.path().join("other")), &executable).is_err());
        assert!(
            user_data(Some(&root.path().join("config.yaml")), None, &executable)
                .unwrap()
                .is_none()
        );
    }
}
