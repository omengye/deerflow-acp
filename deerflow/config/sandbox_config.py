from pydantic import BaseModel, ConfigDict, Field


class VolumeMountConfig(BaseModel):
    """Configuration for a volume mount."""

    host_path: str = Field(..., description="Path on the host machine")
    container_path: str = Field(..., description="Path inside the container")
    read_only: bool = Field(default=False, description="Whether the mount is read-only")


class SandboxConfig(BaseModel):
    """Config section for a sandbox.

    Common options:
        use: Class path of the sandbox provider (required)
        allow_host_bash: Enable bash for host-filesystem-backed providers
            (LocalSandboxProvider and LocalWslProvider). Dangerous and intended
            only for fully trusted local workflows.
        allow_host_tools: Enable tools that execute in the API host process or
            reuse host credentials/login state. Disabled by default.

    AioSandboxProvider specific options:
        image: Docker image to use (default: enterprise-public-cn-beijing.cr.volces.com/vefaas-public/all-in-one-sandbox:latest)
        port: Base port for sandbox containers (default: 8080)
        replicas: Maximum tracked sandbox containers (default: 3). Only idle,
            confirmed resources may be evicted; all-busy capacity is rejected.
        container_prefix: Prefix for container names (default: deer-flow-sandbox)
        idle_timeout: Idle age checked on the next acquire (default: 600 seconds).
            Active or termination-unknown operations remain protected. Zero disables it.
        mounts: List of volume mounts to share directories with the container
        environment: Environment variables to inject into the container (values starting with $ are resolved from host env)
        security_opt: List of Docker --security-opt values to apply to the sandbox container
        container_user: User for docker exec. With 'auto', containers start as the
            image default user and tool commands run as host UID:GID. Explicit
            values are applied to both docker run and docker exec. Null omits
            --user entirely.

    LocalWslProvider specific options (Windows only):
        wsl_distro: WSL distro name (e.g. "Ubuntu-22.04"). If None, the default
            distro is used.
        wsl_user: User to run commands as inside the WSL distro. If None, the
            distro's default user is used.
        wsl_shell: Shell executable inside the distro (default: "bash"). Always
            invoked with the ``-lc`` flag so that PATH/venv setup in the user's
            login shell rc files takes effect.
        wsl_mount_prefix: Mount prefix used by WSL for Windows drives (default:
            "/mnt"). Override if the distro's /etc/wsl.conf customizes
            automount.root.
    """

    use: str = Field(
        ...,
        description="Class path of the sandbox provider (e.g. deerflow.sandbox.local:LocalSandboxProvider)",
    )
    allow_host_bash: bool = Field(
        default=False,
        description=(
            "Allow bash for LocalSandboxProvider or LocalWslProvider. These "
            "providers share the host filesystem and are not isolation boundaries. "
            "Dangerous; intended only for fully trusted local environments."
        ),
    )
    allow_host_tools: bool = Field(
        default=False,
        description=(
            "Allow tools that execute in the API host process or reuse host "
            "credentials/login state (for example host_opencli)."
        ),
    )
    image: str | None = Field(
        default=None,
        description="Docker image to use for the sandbox container",
    )
    port: int | None = Field(
        default=None,
        description="Base port for sandbox containers",
    )
    replicas: int | None = Field(
        default=None,
        ge=1,
        description="Maximum tracked containers (default: 3). Evict only idle resources; reject when all are busy or termination is unknown.",
    )
    container_prefix: str | None = Field(
        default=None,
        description="Prefix for container names",
    )
    idle_timeout: int | None = Field(
        default=None,
        ge=0,
        description="Idle age checked on the next acquire (default: 600 seconds). Active/uncertain operations are protected; 0 disables idle cleanup.",
    )
    mounts: list[VolumeMountConfig] = Field(
        default_factory=list,
        description="List of volume mounts to share directories between host and container",
    )
    environment: dict[str, str] = Field(
        default_factory=dict,
        description="Environment variables to inject into the sandbox container. Values starting with $ will be resolved from host environment variables.",
    )
    security_opt: list[str] = Field(
        default_factory=list,
        description="List of Docker --security-opt values to apply to the sandbox container.",
    )
    container_user: str | None = Field(
        default="auto",
        description=(
            "User for sandbox commands. 'auto' (default) starts the container as the image's "
            "default user, then uses the host process UID:GID for docker exec so files written "
            "to volume-mounted thread directories are owned by the current user rather than root. "
            "Set to an explicit 'UID:GID' string to apply --user to both docker run and docker exec, "
            "or null/empty to omit --user entirely."
        ),
    )

    bash_output_max_chars: int = Field(
        default=20000,
        ge=0,
        description="Maximum characters to keep from bash tool output. Output exceeding this limit is middle-truncated (head + tail), preserving the first and last half. Set to 0 to disable truncation.",
    )
    bash_command_timeout: float = Field(
        default=600,
        gt=0,
        allow_inf_nan=False,
        description="Command execution timeout in seconds for LocalSandbox, WSL and Docker AIO. Transport cleanup has a separate bounded grace period.",
    )
    read_file_output_max_chars: int = Field(
        default=50000,
        ge=0,
        description="Maximum characters in read_file tool output. Built-in providers reduce the byte window and return a continuation; legacy providers use head truncation. Set to 0 to disable the display limit (byte windows remain bounded).",
    )
    ls_output_max_chars: int = Field(
        default=20000,
        ge=0,
        description="Maximum characters in ls tool output. Built-in providers reduce the page size and return a continuation; legacy providers use head truncation. Set to 0 to disable the display limit.",
    )

    wsl_distro: str | None = Field(
        default=None,
        description="WSL distro name for LocalWslProvider (e.g. 'Ubuntu-22.04'). None uses the default distro.",
    )
    wsl_user: str | None = Field(
        default=None,
        description="User to run commands as inside the WSL distro. None uses the distro's default user.",
    )
    wsl_shell: str = Field(
        default="bash",
        description="Shell executable inside the WSL distro. Invoked with '-lc' for login-shell semantics.",
    )
    wsl_mount_prefix: str = Field(
        default="/mnt",
        description="Mount prefix used by WSL for Windows drives. Override if /etc/wsl.conf customizes automount.root.",
    )

    model_config = ConfigDict(extra="allow")
