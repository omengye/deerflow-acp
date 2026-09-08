"""Enroll the Buzz sidecar identity into a membership-gated relay.

The relay owner key is read from Buzz Desktop's Windows Credential Manager
entry. The sidecar key is read from ``.env``. Neither secret nor the minted
invite code is printed or written to disk.
"""

from __future__ import annotations

import argparse
import base64
import getpass
import hashlib
import json
import os
import secrets
import ssl
import subprocess
import sys
import time
import tomllib
from pathlib import Path
from typing import Any

import requests
import win32cred


P = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F
N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141
G = (
    0x79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798,
    0x483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8,
)
BECH32_CHARSET = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
NIP98_KIND = 27235


def _point_add(
    left: tuple[int, int] | None, right: tuple[int, int] | None
) -> tuple[int, int] | None:
    if left is None:
        return right
    if right is None:
        return left
    x1, y1 = left
    x2, y2 = right
    if x1 == x2 and (y1 != y2 or y1 == 0):
        return None
    if left == right:
        slope = (3 * x1 * x1) * pow(2 * y1, P - 2, P) % P
    else:
        slope = (y2 - y1) * pow((x2 - x1) % P, P - 2, P) % P
    x3 = (slope * slope - x1 - x2) % P
    return x3, (slope * (x1 - x3) - y1) % P


def _point_mul(point: tuple[int, int], scalar: int) -> tuple[int, int] | None:
    result = None
    addend: tuple[int, int] | None = point
    while scalar:
        if scalar & 1:
            result = _point_add(result, addend)
        addend = _point_add(addend, addend)
        scalar >>= 1
    return result


def _tagged_hash(tag: str, value: bytes) -> bytes:
    tag_hash = hashlib.sha256(tag.encode("ascii")).digest()
    return hashlib.sha256(tag_hash + tag_hash + value).digest()


def _bech32_decode(value: str) -> tuple[str, list[int]]:
    if value.lower() != value and value.upper() != value:
        raise ValueError("mixed-case Bech32 value")
    value = value.lower()
    separator = value.rfind("1")
    if separator <= 0 or separator + 7 > len(value):
        raise ValueError("invalid Bech32 value")
    hrp = value[:separator]
    try:
        data = [BECH32_CHARSET.index(char) for char in value[separator + 1 :]]
    except ValueError as exc:
        raise ValueError("invalid Bech32 character") from exc

    def expand(prefix: str) -> list[int]:
        return [ord(char) >> 5 for char in prefix] + [0] + [ord(char) & 31 for char in prefix]

    checksum = 1
    for item in expand(hrp) + data:
        high = checksum >> 25
        checksum = (checksum & 0x1FFFFFF) << 5 ^ item
        for index, generator in enumerate(
            [0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3]
        ):
            if (high >> index) & 1:
                checksum ^= generator
    if checksum != 1:
        raise ValueError("invalid Bech32 checksum")
    return hrp, data[:-6]


def _convert_bits(values: list[int], from_bits: int, to_bits: int) -> bytes:
    accumulator = 0
    bits = 0
    output = bytearray()
    maximum = (1 << to_bits) - 1
    for value in values:
        if value < 0 or value >> from_bits:
            raise ValueError("invalid Bech32 data")
        accumulator = (accumulator << from_bits) | value
        bits += from_bits
        while bits >= to_bits:
            bits -= to_bits
            output.append((accumulator >> bits) & maximum)
    if bits >= from_bits or ((accumulator << (to_bits - bits)) & maximum):
        raise ValueError("non-zero Bech32 padding")
    return bytes(output)


def _secret_bytes(value: str) -> bytes:
    value = value.strip()
    if value.startswith("nsec1"):
        hrp, data = _bech32_decode(value)
        if hrp != "nsec":
            raise ValueError("expected an nsec private key")
        raw = _convert_bits(data, 5, 8)
    else:
        raw = bytes.fromhex(value)
    if len(raw) != 32 or not 0 < int.from_bytes(raw, "big") < N:
        raise ValueError("invalid secp256k1 private key")
    return raw


def _public_key(secret: bytes) -> str:
    point = _point_mul(G, int.from_bytes(secret, "big"))
    if point is None:
        raise ValueError("invalid private key")
    return f"{point[0]:064x}"


def _schnorr_sign(message: bytes, secret: bytes) -> str:
    if len(message) != 32:
        raise ValueError("BIP-340 signs a 32-byte message")
    secret_int = int.from_bytes(secret, "big")
    public_point = _point_mul(G, secret_int)
    if public_point is None:
        raise ValueError("invalid private key")
    px, py = public_point
    normalized_secret = secret_int if py % 2 == 0 else N - secret_int
    aux = secrets.token_bytes(32)
    t = bytes(
        left ^ right
        for left, right in zip(
            normalized_secret.to_bytes(32, "big"), _tagged_hash("BIP0340/aux", aux)
        )
    )
    nonce = int.from_bytes(
        _tagged_hash("BIP0340/nonce", t + px.to_bytes(32, "big") + message), "big"
    ) % N
    if nonce == 0:
        raise RuntimeError("BIP-340 generated a zero nonce")
    nonce_point = _point_mul(G, nonce)
    if nonce_point is None:
        raise RuntimeError("BIP-340 generated an invalid nonce point")
    rx, ry = nonce_point
    normalized_nonce = nonce if ry % 2 == 0 else N - nonce
    challenge = int.from_bytes(
        _tagged_hash(
            "BIP0340/challenge",
            rx.to_bytes(32, "big") + px.to_bytes(32, "big") + message,
        ),
        "big",
    ) % N
    signature = rx.to_bytes(32, "big") + (
        (normalized_nonce + challenge * normalized_secret) % N
    ).to_bytes(32, "big")
    return signature.hex()


def _event(secret: bytes, kind: int, tags: list[list[str]], content: str = "") -> dict[str, Any]:
    public_key = _public_key(secret)
    created_at = int(time.time())
    serialized = json.dumps(
        [0, public_key, created_at, kind, tags, content],
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    event_id = hashlib.sha256(serialized).digest()
    return {
        "id": event_id.hex(),
        "pubkey": public_key,
        "created_at": created_at,
        "kind": kind,
        "tags": tags,
        "content": content,
        "sig": _schnorr_sign(event_id, secret),
    }


def _nip98_header(secret: bytes, url: str, body: bytes) -> str:
    event = _event(
        secret,
        NIP98_KIND,
        [
            ["u", url],
            ["method", "POST"],
            ["payload", hashlib.sha256(body).hexdigest()],
            ["nonce", secrets.token_hex(16)],
        ],
    )
    encoded = base64.b64encode(
        json.dumps(event, separators=(",", ":")).encode("utf-8")
    ).decode("ascii")
    return f"Nostr {encoded}"


def _post(secret: bytes, url: str, payload: dict[str, Any]) -> dict[str, Any]:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    response = requests.post(
        url,
        data=body,
        headers={
            "Authorization": _nip98_header(secret, url, body),
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
        timeout=20,
    )
    try:
        data = response.json()
    except ValueError:
        data = {}
    if not response.ok:
        detail = data.get("error") if isinstance(data, dict) else None
        raise RuntimeError(f"HTTP {response.status_code}: {detail or response.reason}")
    if not isinstance(data, dict):
        raise RuntimeError("relay returned a non-object JSON response")
    return data


def _relay_admins(secret: bytes, relay_url: str) -> list[tuple[str, str]]:
    site_packages = Path(__file__).resolve().parent / ".venv" / "Lib" / "site-packages"
    if site_packages.is_dir():
        sys.path.insert(0, str(site_packages))
    from websockets.sync.client import connect

    tls = ssl.create_default_context(cafile=requests.certs.where())
    request_id = f"members-{secrets.token_hex(8)}"
    request = ["REQ", request_id, {"kinds": [13534], "limit": 1}]
    auth_event_id: str | None = None
    deadline = time.monotonic() + 20
    with connect(relay_url, ssl=tls, open_timeout=15, close_timeout=5) as websocket:
        websocket.send(json.dumps(request, separators=(",", ":")))
        while time.monotonic() < deadline:
            message = json.loads(websocket.recv(timeout=max(0.1, deadline - time.monotonic())))
            if not isinstance(message, list) or not message:
                continue
            if message[0] == "AUTH" and len(message) >= 2:
                event = _event(
                    secret,
                    22242,
                    [["relay", relay_url], ["challenge", str(message[1])]],
                )
                auth_event_id = event["id"]
                websocket.send(json.dumps(["AUTH", event], separators=(",", ":")))
            elif message[0] == "OK" and len(message) >= 4 and message[1] == auth_event_id:
                if not message[2]:
                    raise RuntimeError(f"relay authentication failed: {message[3]}")
                websocket.send(json.dumps(request, separators=(",", ":")))
            elif message[0] == "EVENT" and len(message) >= 3:
                event = message[2]
                if not isinstance(event, dict) or event.get("kind") != 13534:
                    continue
                members: list[tuple[str, str]] = []
                for tag in event.get("tags", []):
                    if not isinstance(tag, list) or len(tag) < 2:
                        continue
                    if tag[0] == "member":
                        members.append((str(tag[1]), str(tag[2]) if len(tag) > 2 else "member"))
                    elif tag[0] == "p":
                        members.append((str(tag[1]), str(tag[3]) if len(tag) > 3 else "member"))
                return [item for item in members if item[1] in {"owner", "admin"}]
            elif message[0] == "EOSE":
                return []
        raise RuntimeError("timed out reading the relay membership snapshot")


def _load_env(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8-sig").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def _keyring_secrets() -> dict[str, bytes]:
    credential = win32cred.CredRead(
        "secrets.buzz-desktop", win32cred.CRED_TYPE_GENERIC, 0
    )
    blob = credential["CredentialBlob"]
    if isinstance(blob, bytes):
        text = blob.decode("utf-16-le" if blob.startswith(b"{\x00") else "utf-8")
    else:
        text = str(blob)
    values = json.loads(text.rstrip("\x00"))
    identity = values.get("identity")
    if not isinstance(identity, str) or not identity:
        raise RuntimeError("Buzz Desktop keyring has no identity key")
    result: dict[str, bytes] = {}
    for key, value in values.items():
        if not isinstance(key, str) or not isinstance(value, str):
            continue
        try:
            result[key] = _secret_bytes(value)
        except (ValueError, TypeError):
            continue
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--show-admins", action="store_true")
    parser.add_argument("--admin-key-stdin", action="store_true")
    parser.add_argument("--expected-admin")
    parser.add_argument("--channel-members")
    parser.add_argument("--add-agent-to-channel")
    parser.add_argument("--relay", default="https://buzz.sprwhisp.cc")
    args = parser.parse_args()

    integration_dir = Path(__file__).resolve().parent
    env = _load_env(integration_dir / ".env")
    with (integration_dir / "adapter.toml").open("rb") as config_file:
        config = tomllib.load(config_file)
    agent_value = env.get("BUZZ_PRIVATE_KEY")
    if not agent_value:
        raise RuntimeError("BUZZ_PRIVATE_KEY is missing from .env")
    agent_secret = _secret_bytes(agent_value)
    keyring_secrets = _keyring_secrets()
    desktop_secret = keyring_secrets["identity"]

    expected_agent = str(config["buzz"]["agent_pubkey"])
    expected_owner = str(config["observability"]["owner_pubkey"])
    if _public_key(agent_secret) != expected_agent:
        raise RuntimeError(".env private key does not match the configured sidecar public key")
    if _public_key(desktop_secret) != expected_owner:
        raise RuntimeError("Buzz Desktop identity is not the configured community owner")

    print("Local identity validation: OK")
    if args.dry_run:
        return 0

    provided_secret: bytes | None = None
    if args.admin_key_stdin:
        provided_secret = _secret_bytes(getpass.getpass("Owner/admin private key: "))
        provided_pubkey = _public_key(provided_secret)
        if args.expected_admin and provided_pubkey != args.expected_admin.lower():
            raise RuntimeError("provided private key does not match --expected-admin")

    if args.channel_members or args.add_agent_to_channel:
        if provided_secret is None:
            raise RuntimeError("channel administration requires --admin-key-stdin")
        command = [
            r"D:\Program Files\Buzz\buzz.exe",
            "--relay",
            args.relay,
            "channels",
        ]
        if args.channel_members:
            command.extend(("members", "--channel", args.channel_members))
        else:
            command.extend(
                (
                    "add-member",
                    "--channel",
                    args.add_agent_to_channel,
                    "--pubkey",
                    expected_agent,
                    "--role",
                    "bot",
                )
            )
        child_env = os.environ.copy()
        child_env["BUZZ_PRIVATE_KEY"] = provided_secret.hex()
        child_env.pop("BUZZ_AUTH_TAG", None)
        completed = subprocess.run(
            command,
            env=child_env,
            text=True,
            capture_output=True,
            timeout=60,
            check=False,
        )
        output = completed.stdout.strip() or completed.stderr.strip()
        if output:
            print(output)
        if completed.returncode:
            raise RuntimeError(f"Buzz CLI exited with code {completed.returncode}")
        return 0

    if args.show_admins:
        relay_ws = args.relay.replace("https://", "wss://", 1).replace("http://", "ws://", 1)
        admins = _relay_admins(desktop_secret, relay_ws)
        if not admins:
            print("Relay membership snapshot contains no visible owner/admin entries")
        else:
            for pubkey, role in admins:
                print(f"Relay {role}: {pubkey}")
        return 0

    base = args.relay.rstrip("/")
    candidate_secrets = keyring_secrets
    if provided_secret is not None:
        candidate_secrets = {"provided-owner": provided_secret}

    minted: dict[str, Any] | None = None
    minting_key = ""
    rejected = 0
    for key_name, candidate_secret in candidate_secrets.items():
        if _public_key(candidate_secret) == expected_agent:
            continue
        try:
            print(f"Checking local identity: {key_name}", flush=True)
            minted = _post(
                candidate_secret,
                f"{base}/api/invites",
                {"ttl_secs": 300, "max_uses": 1},
            )
            minting_key = key_name
            break
        except RuntimeError as exc:
            if str(exc).startswith("HTTP 403:"):
                rejected += 1
                print(f"Not an owner/admin: {key_name}", flush=True)
                continue
            raise
    if minted is None:
        raise RuntimeError(
            f"none of the {rejected} local Buzz identities has owner/admin permission"
        )
    code = minted.get("code")
    if not isinstance(code, str) or not code:
        raise RuntimeError("relay did not return an invite code")
    result = _post(
        agent_secret,
        f"{base}/api/invites/claim",
        {"code": code},
    )
    status = result.get("status")
    role = result.get("role")
    if status not in {"joined", "already_member"}:
        raise RuntimeError(f"unexpected claim result: status={status!r}")
    print(f"Relay enrollment: {status} (role={role}, authorized_by={minting_key})")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"Enrollment failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
