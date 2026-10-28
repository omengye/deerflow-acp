# BuildKit requires a local image name. Verify this name resolves to BASE_ID in release.py.
ARG BASE_IMAGE=deerflow-server:connection-step1-20261028
FROM ${BASE_IMAGE}
USER root
# The existing image entrypoint and the production bind-mounted wrapper remain intact.
COPY --chmod=0755 bin/deerflow-acpd.real /opt/deerflow/bin/deerflow-acpd.real
COPY --chmod=0755 bin/deerflow-config-go /opt/deerflow/bin/deerflow-config-go
COPY --chmod=0755 bin/deerflow-control-go /opt/deerflow/bin/deerflow-control-go
COPY --chmod=0755 bin/deerflow-skill-go /opt/deerflow/bin/deerflow-skill-go
ENV DEER_FLOW_ACP_DAEMON_STOP_TIMEOUT_MS=60000
LABEL cc.sprwhisp.deerflow.rollout="skill-learning-recovery-20261028"
USER deerflow:deerflow
