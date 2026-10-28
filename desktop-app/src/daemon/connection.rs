//! Local desktop connection preferences. Credentials never cross settings RPC.

use std::fmt;
use std::path::{Path, PathBuf};

use anyhow::{Context, bail};
use base64::Engine as _;
use serde::{Deserialize, Serialize};

#[derive(Clone, Default, PartialEq, Eq)]
pub struct ConnectionSettings {
    pub remote: bool,
    pub address: String,
    pub token: String,
}

impl fmt::Debug for ConnectionSettings {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ConnectionSettings")
            .field("remote", &self.remote)
            .field("address", &self.address)
            .field("token", &"[redacted]")
            .finish()
    }
}

#[derive(Serialize, Deserialize)]
struct StoredSettings {
    #[serde(default)]
    remote: bool,
    #[serde(default)]
    address: String,
    #[serde(default)]
    credential: Option<StoredCredential>,
}

#[derive(Serialize, Deserialize)]
struct StoredCredential {
    protection: String,
    value: String,
}

pub fn normalize_address(address: &str) -> anyhow::Result<String> {
    let mut url = url::Url::parse(address.trim()).context("服务器地址格式无效")?;
    if !matches!(url.scheme(), "ws" | "wss") || url.host().is_none() {
        bail!("服务器地址必须以 ws:// 或 wss:// 开头，并包含主机地址");
    }
    if !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        bail!("请在 Token 输入框填写凭据，服务器地址不能包含凭据、查询参数或片段");
    }
    if !matches!(url.path(), "" | "/" | "/v1" | "/v1/") || url.port() == Some(0) {
        bail!("服务器地址只支持主机、端口及 /v1 路径");
    }
    url.set_path("/v1");
    Ok(url.to_string())
}

impl ConnectionSettings {
    pub fn validate(&self) -> anyhow::Result<Self> {
        let mut settings = self.clone();
        if self.remote {
            settings.address = normalize_address(&self.address)?;
            if self.token.trim().is_empty()
                || self.token.len() > 4096
                || self.token.chars().any(char::is_whitespace)
            {
                bail!("请填写有效的服务器 Token，Token 不能包含空白字符");
            }
        }
        Ok(settings)
    }

    pub fn default_path() -> PathBuf {
        waku_client::identity::desktop_data_directory().join("server-connection.json")
    }

    pub fn load() -> anyhow::Result<Self> {
        Self::load_from(&Self::default_path())
    }

    pub fn load_from(path: &Path) -> anyhow::Result<Self> {
        let bytes = match std::fs::read(path) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Self::default());
            }
            Err(error) => return Err(error).context("无法读取服务器连接设置"),
        };
        if bytes.len() > 64 * 1024 {
            bail!("服务器连接设置文件过大");
        }
        let stored: StoredSettings =
            serde_json::from_slice(&bytes).context("服务器连接设置损坏")?;
        let token = stored
            .credential
            .map(unprotect)
            .transpose()?
            .unwrap_or_default();
        Self {
            remote: stored.remote,
            address: stored.address,
            token,
        }
        .validate()
    }

    pub fn save(&self) -> anyhow::Result<()> {
        self.save_to(&Self::default_path())
    }

    pub fn save_to(&self, path: &Path) -> anyhow::Result<()> {
        let settings = self.validate()?;
        let stored = StoredSettings {
            remote: settings.remote,
            address: settings.address,
            credential: (!settings.token.is_empty())
                .then(|| protect(settings.token.as_bytes()))
                .transpose()?,
        };
        let directory = path.parent().context("服务器连接设置目录无效")?;
        std::fs::create_dir_all(directory)?;
        let temporary = directory.join(format!(".server-connection-{}.tmp", uuid::Uuid::new_v4()));
        let result = (|| -> anyhow::Result<()> {
            use std::io::Write as _;
            let mut options = std::fs::OpenOptions::new();
            options.write(true).create_new(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt as _;
                options.mode(0o600);
            }
            let mut file = options.open(&temporary)?;
            file.write_all(&serde_json::to_vec_pretty(&stored)?)?;
            file.sync_all()?;
            drop(file);
            std::fs::rename(&temporary, path)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = std::fs::remove_file(&temporary);
        }
        result.context("无法保存服务器连接设置")
    }
}

fn protect(bytes: &[u8]) -> anyhow::Result<StoredCredential> {
    #[cfg(windows)]
    let (protection, bytes) = ("windows-dpapi", dpapi(bytes, true)?);
    #[cfg(not(windows))]
    let (protection, bytes) = ("owner-only-file", bytes.to_vec());
    Ok(StoredCredential {
        protection: protection.into(),
        value: base64::engine::general_purpose::STANDARD.encode(bytes),
    })
}

fn unprotect(credential: StoredCredential) -> anyhow::Result<String> {
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(credential.value)
        .context("保存的 Token 已损坏")?;
    #[cfg(windows)]
    let bytes = {
        if credential.protection != "windows-dpapi" {
            bail!("保存的 Token 不适用于当前系统，请重新填写");
        }
        dpapi(&bytes, false)?
    };
    #[cfg(not(windows))]
    if credential.protection != "owner-only-file" {
        bail!("保存的 Token 不适用于当前系统，请重新填写");
    }
    String::from_utf8(bytes).context("保存的 Token 编码无效")
}

#[cfg(windows)]
fn dpapi(bytes: &[u8], encrypt: bool) -> anyhow::Result<Vec<u8>> {
    use windows_sys::Win32::Foundation::LocalFree;
    use windows_sys::Win32::Security::Cryptography::{
        CRYPT_INTEGER_BLOB, CRYPTPROTECT_UI_FORBIDDEN, CryptProtectData, CryptUnprotectData,
    };
    let input = CRYPT_INTEGER_BLOB {
        cbData: bytes.len().try_into()?,
        pbData: bytes.as_ptr().cast_mut(),
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: std::ptr::null_mut(),
    };
    let success = unsafe {
        if encrypt {
            CryptProtectData(
                &input,
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        } else {
            CryptUnprotectData(
                &input,
                std::ptr::null_mut(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        }
    };
    if success == 0 {
        return Err(std::io::Error::last_os_error())
            .context("无法访问当前 Windows 用户的 Token 加密凭据");
    }
    let result =
        unsafe { std::slice::from_raw_parts(output.pbData, output.cbData as usize).to_vec() };
    unsafe {
        LocalFree(output.pbData.cast());
    }
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoint_validation_preserves_only_the_supported_protocol_endpoint() {
        assert_eq!(
            normalize_address(" ws://192.168.1.190:18787 ").unwrap(),
            "ws://192.168.1.190:18787/v1"
        );
        assert_eq!(
            normalize_address("wss://example.com/v1/").unwrap(),
            "wss://example.com/v1"
        );
        assert_eq!(
            normalize_address("ws://[::1]:18787").unwrap(),
            "ws://[::1]:18787/v1"
        );
        for address in [
            "http://server",
            "ws://user:secret@server",
            "ws://server?token=secret",
            "ws://server/#secret",
            "ws://server/other",
            "ws://server:0",
        ] {
            assert!(normalize_address(address).is_err());
        }
    }

    #[test]
    fn credentials_round_trip_and_mode_changes_keep_the_saved_server() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("connection.json");
        let mut settings = ConnectionSettings {
            remote: true,
            address: "ws://127.0.0.1:18787/v1".into(),
            token: "test-private-token".into(),
        };
        settings.save_to(&path).unwrap();
        assert_eq!(ConnectionSettings::load_from(&path).unwrap(), settings);
        assert!(
            !std::fs::read_to_string(&path)
                .unwrap()
                .contains("test-private-token")
        );
        assert!(!format!("{settings:?}").contains("test-private-token"));
        settings.remote = false;
        settings.save_to(&path).unwrap();
        assert_eq!(ConnectionSettings::load_from(&path).unwrap(), settings);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            assert_eq!(
                std::fs::metadata(path).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
    }

    #[test]
    fn missing_and_corrupt_preferences_are_distinguished() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("connection.json");
        assert_eq!(
            ConnectionSettings::load_from(&path).unwrap(),
            ConnectionSettings::default()
        );
        std::fs::write(&path, "{").unwrap();
        assert!(ConnectionSettings::load_from(&path).is_err());
    }
}
