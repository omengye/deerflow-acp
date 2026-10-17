# Skill authoring and deployment safeguards

## Incident evidence

The Huawei skill was created with a UTF-8 BOM. Startup discovery and installation
previously required a delimiter at byte zero. The author then used direct SQL
against `config/go-harness-state/harness.db`, while the live daemon owned
`data/go-harness/harness.db`. A script's printed success was mistaken for a host
publication result. The profile's builtin creator also lagged behind resources.

## Changes

- Discovery uses the installation parser, accepting one leading BOM and CRLF.
  Only the parsing view changes; stored bytes, manifests and hashes do not.
- Malformed discovered documents produce bounded path/error diagnostics in the
  host catalog and desktop skill-source settings instead of silently vanishing.
- Each daemon exposes a fresh non-secret instance ID and canonical data directory.
  Go control and Rust Bridge bind requests and responses to that instance. The
  control CLI requires an explicit runtime directory (or its named environment
  variable), and supports `--expect-instance-id` across multiple operations.
- `skill.verify` requires a catalog target and existing foreground session. It
  resolves that session's next-turn selection and optionally checks an expected
  version before reading the body. It neither executes support scripts nor
  installs a missing skill. Publication and availability remain separate facts.
- `skill_manage get` requires a proposal ID. Its host result carries source,
  state-directory and next-turn availability evidence. Host instructions and
  skill-creator forbid direct database fallback and require capability checks.
- Candidate Python restrictions remain unchanged. Installed-structure validation
  is not candidate eligibility or publication approval.
- Profile initialization reports builtin/resource drift without replacing local
  edits. During a controlled update, an operator can review and replace the
  creator only with an expected file hash and a complete prior backup.
- The updater drains an idle host, stops only processes from the target package,
  backs up all user data and programs, verifies hashes, and replaces all nine
  EXEs plus resources. It refuses reparse-point packages and preexisting backup
  destinations. Replacement errors restore original programs, never roll back
  live user databases. The reviewed creator is the only optional user-data edit.
- A running bundled WeChat channel pins its executable hash. After reviewing
  that registration, pass `-Restart -RefreshBundledWeChat` to the updater. It
  re-registers only the reviewed bundled executable through instance-bound
  `channel.update`, preserving account, workspace, permissions and options.
  It does not write SQLite directly or disable plugin checksum validation.
  Custom plugin registrations must be reviewed separately, never silently
  redirected to a bundled executable. Disabled bundled channels also need this
  explicit refresh before they are enabled after an executable replacement.

## Boundaries

These are workflow, protocol and validation safeguards, not OS isolation. An
unrestricted shell running as the same Windows user can still write files that
user owns. Hard database write isolation requires a separately sandboxed tool
identity/ACL design; substring command filtering is not a security boundary.
Existing checkpoint executions keep their pinned skill snapshots. The verifier
explicitly checks the next normal turn, not a suspended execution or remote SSH.

Upgrade tests use synthetic user data and local fake model endpoints. A passing
smoke test does not establish remote server reachability or script correctness.
