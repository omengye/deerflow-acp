//! Daemon-only provider discovery layered over shared protocol models.

use std::path::Path;

pub use waku_protocol::model::*;

pub fn provider_probe(provider: ProviderKind, binary_override: Option<&str>) -> ProviderProbe {
    let path = match (provider.is_available(), binary_override) {
        (false, _) => None,
        (true, Some(binary)) => crate::command_env::resolve_binary_override(binary),
        (true, None) => waku_protocol::identity::bundled_acp_binary()
            .or_else(|| crate::command_env::find_executable(DEERFLOW_ACP_COMMAND)),
    };
    ProviderProbe {
        provider,
        installed: path.is_some(),
        path,
        models: Vec::new(),
        agent_presets: Vec::new(),
    }
}

pub fn discover_provider_models(mut probe: ProviderProbe) -> ProviderProbe {
    if probe.provider.supports_model_discovery()
        && let Some(path) = probe.path.as_deref()
    {
        probe.models = crate::model_catalog::discover_deerflow_models(path);
    }
    probe
}

/// Run `<cli> --version` on the daemon host and extract its first version-like
/// token. Provider CLIs decorate this output differently, so clients receive a
/// normalized value rather than subprocess output.
pub fn probe_provider_version(binary: &Path) -> Option<String> {
    let mut command = crate::command_env::command(binary);
    let command = command.arg("--version").stdin(std::process::Stdio::null());
    let output = crate::command_env::output(command).ok()?;
    let combined = format!(
        "{}\n{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    parse_cli_version(&combined)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn retired_provider_never_resolves_an_override() {
        let binary = std::env::current_exe().unwrap();
        let probe = provider_probe(ProviderKind::Codex, binary.to_str());
        assert!(!probe.installed);
        assert!(probe.path.is_none());
        assert!(probe.models.is_empty());
    }
}
