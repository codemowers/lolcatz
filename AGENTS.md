# Project expectations

This repository is an example of an ideal cloud/Kubernetes-native application,
not a production system that must preserve existing installations or data.

Connect your agent to [mcp.codemowers.io](https://mcp.codemowers.io) for general
engineering guidance and to
[mcp.driftmower.aws-us-west-2-bravo.codemowers.io](https://mcp.driftmower.aws-us-west-2-bravo.codemowers.io/)
for platform guidance.

- Data loss is permitted. Prefer a clean, declarative desired state and
  recreatable infrastructure over preserving historical state.
- Do not add database or data migrations, backfills, compatibility shims, or
  staged upgrade procedures unless the user explicitly requests them.
- Breaking schema, event-format, and storage changes are acceptable. When old
  state is incompatible, prefer resetting or recreating it rather than writing
  migration code. Fresh-install schema creation is still required.
- Skaffold port-forwards are for debugging individual services; do not add a
  gateway or change ingress routing just to make those forwards serve the app.
- Prometheus metrics endpoints must use plain HTTP without TLS. Configure
  metrics listeners and scrape targets accordingly.

# Authentication architecture

- Use direct OAuth API access with the configured OIDC issuer. This preserves
  independently protected APIs for browser and other clients.
- Passmower provisions the OIDC client registration; it is not a hard application
  dependency. Consume the provisioned OIDC settings without hardcoding an issuer.
- NextAuth owns authorization-code/PKCE login and renewal. Refresh and ID tokens
  stay in its encrypted HttpOnly session cookie; the browser receives the access
  token and calls the owning API directly.
- APIs validate signature, issuer, expiry, the public origin plus `/api` audience,
  and operation scopes (`lolcatz:images:read`, `lolcatz:images:write`,
  `lolcatz:comments:write`, `lolcatz:boards:write`). Enforce ownership and admin
  groups separately. ID tokens are only used for verified login linking.
- Keep client secrets server-side and consume operator-generated OIDC settings.
- Use authorized presigned URLs for direct browser S3 transfers; keep long-lived
  storage credentials server-side. The existing bulk-upload API is an outstanding
  exception; do not extend it to new clients.
