# Add skill runtimes to the deployed server; reuse all original Go/Rust binaries,
# portable initialization, health check, volumes and container entrypoint.
FROM deerflow-server:pilot-20261024

USER root
ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=http://deb.debian.org/debian-security
ARG PIP_INDEX_URL=https://pypi.org/simple
RUN sed -i "s|http://deb.debian.org/debian-security|${DEBIAN_SECURITY_MIRROR}|g; s|http://deb.debian.org/debian|${DEBIAN_MIRROR}|g" /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        python3 \
        python3-venv \
        nodejs \
        npm \
        curl \
    && python3 -m venv /opt/deerflow-skill-python \
    && /opt/deerflow-skill-python/bin/python -m pip install --no-cache-dir 'paramiko>=3.4,<5' \
    && /opt/deerflow-skill-python/bin/python -m pip check \
    && rm -rf /var/lib/apt/lists/*

# Verify the exact allowlisted paths. The backend resolves aliases on launch;
# preserving the venv invocation path is required for Python package discovery.
RUN set -eu; \
    for executable in \
        /opt/deerflow-skill-python/bin/python /usr/bin/python3.11 \
        /usr/bin/node /usr/bin/npm /usr/bin/npx /usr/bin/curl /usr/bin/git \
        /bin/bash /usr/bin/tar /usr/bin/find /usr/bin/mktemp; do \
        test -x "$executable"; \
    done \
    && test "$(readlink -f /opt/deerflow-skill-python/bin/python)" = /usr/bin/python3.11 \
    && /opt/deerflow-skill-python/bin/python -c "import importlib.metadata,sys,paramiko; assert sys.version_info[:2] == (3,11); assert sys.prefix == '/opt/deerflow-skill-python'; print('Python',sys.version.split()[0]); print('paramiko',importlib.metadata.version('paramiko'))" \
    && node -e "if (Number(process.versions.node.split('.')[0]) < 18) process.exit(1); console.log('Node',process.versions.node)" \
    && npm --version \
    && curl --version \
    && mv /opt/deerflow/bin/deerflow-acpd /opt/deerflow/bin/deerflow-acpd.real

COPY --chmod=0755 runtime/deerflow-acpd-wrapper.sh /opt/deerflow/bin/deerflow-acpd
RUN /bin/sh -n /opt/deerflow/bin/deerflow-acpd

USER deerflow:deerflow
