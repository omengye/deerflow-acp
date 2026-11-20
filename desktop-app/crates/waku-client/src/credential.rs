//! Encrypt the fields on the client before responding to the pending ACP approval.
use anyhow::{Result, anyhow, ensure};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use ring::{
    aead, agreement, hkdf,
    rand::{SecureRandom, SystemRandom},
};
use waku_protocol::model::{CredentialForm, SealedCredential};
use zeroize::Zeroizing;

const LABEL: &str = "DeerFlow service credential v2";
// RFC 5480 SubjectPublicKeyInfo for id-ecPublicKey + prime256v1, uncompressed.
const SPKI: &[u8] = &[
    0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08, 0x2a,
    0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00,
];

pub fn fields(form: &CredentialForm) -> Result<(bool, bool)> {
    let fields = form.required_fields.as_slice();
    ensure!(form.version == 2, "此凭据表单需要更新桌面客户端");
    match fields {
        [u, k] if u == "username" && k == "key" => Ok((true, true)),
        [u] if u == "username" => Ok((true, false)),
        [k] if k == "key" => Ok((false, true)),
        _ => Err(anyhow!("无法识别凭据表单要求的字段")),
    }
}

pub fn seal(form: &CredentialForm, username: &str, key: &str) -> Result<SealedCredential> {
    let (user_required, key_required) = fields(form)?;
    for (required, value) in [(user_required, username), (key_required, key)] {
        ensure!(
            required == !value.is_empty() && value.len() <= 65536 && !value.contains('\0'),
            "请填写表单要求的字段，每项最多 64 KiB"
        );
    }
    ensure!(form.public_key.len() == 124, "凭据表单的公钥无效");
    let der = STANDARD
        .decode(&form.public_key)
        .map_err(|_| anyhow!("凭据表单的公钥无效"))?;
    let point = der
        .strip_prefix(SPKI)
        .filter(|p| p.len() == 65 && p[0] == 4)
        .ok_or_else(|| anyhow!("凭据表单的公钥无效"))?;
    let peer = agreement::UnparsedPublicKey::new(&agreement::ECDH_P256, point);
    let rng = SystemRandom::new();
    let failed = |_| anyhow!("无法加密本次凭据");
    let ephemeral =
        agreement::EphemeralPrivateKey::generate(&agreement::ECDH_P256, &rng).map_err(failed)?;
    let public = ephemeral.compute_public_key().map_err(failed)?;
    let aead_key = agreement::agree_ephemeral(ephemeral, &peer, |shared| {
        let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, &[]).extract(shared);
        let info = [LABEL.as_bytes()];
        let okm = prk.expand(&info, &aead::AES_256_GCM)?;
        Ok::<_, ring::error::Unspecified>(aead::LessSafeKey::new(okm.into()))
    })
    .map_err(failed)?
    .map_err(failed)?;
    let mut nonce = [0; 12];
    rng.fill(&mut nonce).map_err(failed)?;
    let fields = form.required_fields.join(",");
    let aad = [LABEL, &form.consumer, &form.service, &form.account, &fields].join("\0");
    let mut data = Zeroizing::new(Vec::with_capacity(4 + username.len() + key.len() + 16));
    data.extend_from_slice(&(username.len() as u32).to_be_bytes());
    data.extend_from_slice(username.as_bytes());
    data.extend_from_slice(key.as_bytes());
    aead_key
        .seal_in_place_append_tag(
            aead::Nonce::assume_unique_for_key(nonce),
            aead::Aad::from(aad.as_bytes()),
            &mut *data,
        )
        .map_err(failed)?;
    Ok(SealedCredential {
        public_key: form.public_key.clone(),
        client_key: STANDARD.encode([SPKI, public.as_ref()].concat()),
        nonce: STANDARD.encode(nonce),
        ciphertext: STANDARD.encode(data.as_slice()),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn form(names: &[&str], public_key: String) -> CredentialForm {
        CredentialForm {
            version: 2,
            required_fields: names.iter().map(|name| name.to_string()).collect(),
            public_key,
            consumer: "example-tool".into(),
            service: "example-service".into(),
            account: "default".into(),
        }
    }

    #[test]
    fn credential_packet_authenticates_scope_and_only_requested_fields() {
        for (names, username, key) in [
            (vec!["username", "key"], "测试用户", "synthetic password"),
            (vec!["key"], "", "synthetic token"),
            (vec!["username"], "测试用户", ""),
        ] {
            let private = agreement::EphemeralPrivateKey::generate(
                &agreement::ECDH_P256,
                &SystemRandom::new(),
            )
            .unwrap();
            let public = private.compute_public_key().unwrap();
            let form = form(&names, STANDARD.encode([SPKI, public.as_ref()].concat()));
            let reply = seal(&form, username, key).unwrap();
            assert_eq!(reply.public_key, form.public_key);
            let der = STANDARD.decode(reply.client_key).unwrap();
            let peer = agreement::UnparsedPublicKey::new(
                &agreement::ECDH_P256,
                der.strip_prefix(SPKI).unwrap(),
            );
            let secret = agreement::agree_ephemeral(private, &peer, |shared| {
                let prk = hkdf::Salt::new(hkdf::HKDF_SHA256, &[]).extract(shared);
                aead::LessSafeKey::new(
                    prk.expand(&[b"DeerFlow service credential v2"], &aead::AES_256_GCM)
                        .unwrap()
                        .into(),
                )
            })
            .unwrap();
            let nonce: [u8; 12] = STANDARD.decode(reply.nonce).unwrap().try_into().unwrap();
            let aad = format!(
                "DeerFlow service credential v2\0example-tool\0example-service\0default\0{}",
                names.join(",")
            );
            let mut encrypted = STANDARD.decode(reply.ciphertext).unwrap();
            assert!(
                secret
                    .open_in_place(
                        aead::Nonce::assume_unique_for_key(nonce),
                        aead::Aad::from(b"other account"),
                        &mut encrypted.clone()
                    )
                    .is_err()
            );
            let decoded = secret
                .open_in_place(
                    aead::Nonce::assume_unique_for_key(nonce),
                    aead::Aad::from(aad.as_bytes()),
                    &mut encrypted,
                )
                .unwrap();
            let n = u32::from_be_bytes(decoded[..4].try_into().unwrap()) as usize;
            assert_eq!(&decoded[4..4 + n], username.as_bytes());
            assert_eq!(&decoded[4 + n..], key.as_bytes());
        }
    }

    #[test]
    fn credential_form_rejects_missing_extra_or_unknown_fields() {
        for names in [
            vec![],
            vec!["username", "username"],
            vec!["key", "username"],
            vec!["unknown"],
        ] {
            assert!(fields(&form(&names, String::new())).is_err());
        }
        let private =
            agreement::EphemeralPrivateKey::generate(&agreement::ECDH_P256, &SystemRandom::new())
                .unwrap();
        let public = private.compute_public_key().unwrap();
        let single = form(&["key"], STANDARD.encode([SPKI, public.as_ref()].concat()));
        for (user, key) in [("", ""), ("unexpected", "key"), ("", "a\0b")] {
            assert!(seal(&single, user, key).is_err());
        }
        let old = CredentialForm {
            version: 1,
            ..single
        };
        assert!(fields(&old).is_err());
    }
}
