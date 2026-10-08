# DIGIT 2026: Paws or Claws

This workshop extends Lolcatz with authenticated thumbs-up and thumbs-down
voting. Votes are stored in PostgreSQL and published through MQTT so every
browser viewing an image sees updated totals without refreshing.

## 1. Prerequisites

1. Install the required command-line tools:

   - [Git](https://git-scm.com/downloads)
   - [Docker Engine or Docker Desktop](https://docs.docker.com/engine/install/)
   - [kubectl](https://kubernetes.io/docs/tasks/tools/)
   - [kubelogin (`kubectl oidc-login`)](https://github.com/int128/kubelogin#setup)
   - [Helm](https://helm.sh/docs/intro/install/)
   - [Skaffold](https://skaffold.dev/docs/install/)

   Verify the installation:

   ```bash
   git --version
   docker version
   kubectl version --client
   kubectl oidc-login --help
   helm version
   skaffold version
   ```
2. Clone this repository from GitHub.
   ```bash
   git clone git@github.com:<github-user>/lolcatz.git
   cd lolcatz
   ```

3. Go to [trial.codemowers.io](https://trial.codemowers.io/) and sign in. This will allow you to create a fresh sandbox environment for this workshop.

4. Go to the [Provisioning tab](https://driftmower.aws-us-west-2-bravo.codemowers.io/provisioning) and download the sandbox kubeconfig from Driftmower from and configure it as described
   in [Kubernetes' kubeconfig documentation](https://kubernetes.io/docs/concepts/configuration/organize-cluster-access-kubeconfig/).
   The kubeconfig uses `kubectl oidc-login`; the first command may open a browser:

   ```bash
   kubectl config get-contexts
   kubectl config use-context <sandbox-context>
   kubectl config view --minify --output 'jsonpath={..namespace}'
   kubectl get pods
   ```
   You should see the following pods that are provisioned automatically

   ```bash
   NAME                      READY   STATUS    RESTARTS   AGE
   prometheus-sandbox-0      2/2     Running   0          30h
   registry-8775f55b-g6lmd   1/1     Running   0          40m
   ```

## 2. Deploying with Skaffold

1. Create the ignored, sandbox-specific Skaffold environment file:

   ```bash
   cp skaffold.env.example skaffold.env
   ```

   Replace the placeholders in `skaffold.env`:

   ```dotenv
   SKAFFOLD_KUBE_CONTEXT=<sandbox-context>
   SKAFFOLD_NAMESPACE=<sandbox-namespace>
   SKAFFOLD_DEFAULT_REPO=<registry-host>
   ```

   Copy the registry host from Driftmower's
   [Provisioning → Container registry](https://driftmower.aws-us-west-2-bravo.codemowers.io/provisioning#registry)
   section; it is `registry.<sandbox-namespace>.aws-us-west-2-bravo.codemowers.io`.
   Do not append anything to it. Skaffold reads `skaffold.env` automatically;
   other shell commands do not. Never commit `skaffold.env` or registry
   credentials.

   Nothing else is sandbox-specific. The chart's hostnames are short names
   such as `can-i-haz-kubernetes`; an admission policy expands them to
   `<name>.<sandbox-namespace>.aws-us-west-2-bravo.codemowers.io` and
   cert-manager issues their certificates from the Ingress.

2. Load the same values into the current shell when running the remaining setup
   commands:

   ```bash
   set -a
   source skaffold.env
   set +a
   ```


3. Log Docker in to the sandbox registry. Paste the `docker login` line from
   the same
   [Provisioning → Container registry](https://driftmower.aws-us-west-2-bravo.codemowers.io/provisioning#registry)
   section; it carries the sandbox's robot credential. Expect
   **Login Succeeded**.

4. Validate the generated manifests before changing the cluster:

   ```bash
   helm lint ./chart
   skaffold render --digest-source=none --offline \
     --output /tmp/lolcatz.yaml
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     create --dry-run=server -f /tmp/lolcatz.yaml
   ```



5. Start the development loop from the repository root:

   ```bash
   skaffold dev
   ```

   Skaffold builds the images, pushes them to the sandbox registry, deploys the
   Helm chart, tails logs, and rebuilds changed services. Keep it running while
   working. Use Skaffold's port-forwards only to debug individual services.

   Open the application at the hostname the Ingress resolved to:

   ```bash
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     get ingress lolcatz-frontend -o jsonpath='https://{.spec.rules[0].host}{"\n"}'
   ```

   The first page load can take a minute while Let's Encrypt issues the
   certificate.

6. In another terminal, load `skaffold.env` and inspect the deployment:

   ```bash
   set -a
   source skaffold.env
   set +a

   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get pods
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get ingress,certificate
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get events \
     --sort-by=.metadata.creationTimestamp
   ```

## 3. Making changes

1. Create the `image_votes` table in the voting service's fresh-install schema:

   ```sql
   CREATE TABLE image_votes (
       image_id   TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
       voter_id   TEXT NOT NULL,
       vote       SMALLINT NOT NULL CHECK (vote IN (-1, 1)),
       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
       PRIMARY KEY (image_id, voter_id)
   );
   ```

   Use `1` for 👍 and `-1` for 👎. The primary key is the final authority when
   duplicate requests arrive concurrently; do not implement a read-before-write
   check.

2. In the starter Sanic service, implement
   `POST /api/voting/images/{image_id}/vote`:

   - require `Authorization: Bearer <access-token>`;
   - verify token signature, issuer, expiry, the public origin plus `/api`
     audience, and the required scope;
   - derive `voter_id` from the verified `iss` and `sub` claims;
   - accept only `{"vote":"up"}` or `{"vote":"down"}`;
   - insert and commit the vote before publishing; and
   - return `201`, or `409` for a duplicate primary key.

   Never accept a voter identity from the request body, an IP address, a cookie,
   or `localStorage`.

3. Implement public `GET /api/voting/images/{image_id}/votes` using conditional
   aggregation:

   ```sql
   SELECT
       COUNT(*) FILTER (WHERE vote = 1)  AS up,
       COUNT(*) FILTER (WHERE vote = -1) AS down
   FROM image_votes
   WHERE image_id = $1;
   ```

   Both vote endpoints return:

   ```json
   {"image_id":"01JABC123","up":12,"down":3}
   ```

4. Add `GET /api/voting/healthz`, the Python dependencies, a Docker image,
   and the voting service's Deployment and Service. Take PostgreSQL and OIDC
   settings from the Secrets the operators already provision in the
   namespace, as the other services do; do not hardcode credentials or the
   issuer:

   | Secret | Key | Holds |
   |---|---|---|
   | `lolcatz-database-app` | `uri` | PostgreSQL connection URL (CloudNativePG) |
   | `oidc-client-lolcatz-frontend-owner-secrets` | `OIDC_IDP_URI` | the OIDC issuer, for token signature and `iss` checks |
   | `oidc-client-lolcatz-frontend-owner-secrets` | `OIDC_CLIENT_ORIGIN` | the public origin; the access token audience is this plus `/api` |

   Do not add a `securityContext` for the namespace's `restricted` Pod
   Security Standard: the platform's admission policies set seccomp, dropped
   capabilities, `allowPrivilegeEscalation` and user-namespace isolation on
   every pod, so the image may even run as root inside its user namespace.
   Set only `readOnlyRootFilesystem: true` on the container, as the other
   services do, and write to an `emptyDir` mounted at `/tmp`.

5. Add the voting image to `skaffold.yaml`. Keep its image name short, such as
   `lolcatz-voting`; Skaffold prefixes it with `SKAFFOLD_DEFAULT_REPO`.

6. Route `/api/voting` to the voting Service in
   `chart/templates/frontend-ingress.yaml`. Preserve the path: services in this
   repository handle their full `/api/<service>` paths without an Ingress
   rewrite.

7. Declare the application-owned EMQX resources in the Helm chart; do not
   install the cluster-wide operator:

   - an `apps.emqx.io/v2` `EMQX` resource with an MQTT-over-WebSocket listener;
   - a Service for the listener;
   - publisher credentials in a Secret;
   - authorization that permits anonymous subscription to
     `lolcatz/images/+/votes` but denies anonymous publication; and
   - a standard `networking.k8s.io/v1` Ingress with the short host `mqtt`,
     routing `/mqtt` to the listener Service.

   TLS terminates at the Ingress. Nothing else is needed: Traefik is the
   default ingress class, the admission policy expands `mqtt` to
   `mqtt.<sandbox-namespace>.aws-us-west-2-bravo.codemowers.io`, and
   cert-manager issues the certificate from the Ingress. Sandboxes reject
   Traefik `IngressRoute` resources and wildcard certificates.

8. After a successful commit, publish the new totals to
   `lolcatz/images/{image_id}/votes` with QoS 1 and `retain=true`. PostgreSQL is
   authoritative; MQTT is only the notification channel.

9. Add the vote buttons and MQTT.js client to the frontend. Connect to
   `wss://mqtt.<sandbox-namespace>.aws-us-west-2-bravo.codemowers.io/mqtt`
   (`kubectl get ingress` shows the resolved host), subscribe only to the
   displayed image's topic, and fall back to the HTTP GET endpoint after
   connection or decoding failures.

10. Follow the existing direct OAuth pattern: NextAuth owns login and renewal,
   the browser sends its access token directly to the voting API, and the API
   validates it. Keep refresh tokens, ID tokens, client secrets, database
   credentials, and MQTT publisher credentials out of browser code.

11. Save each working increment on your exercise branch:

    ```bash
    git switch -c paws-or-claws
    git status --short
    git add services chart skaffold.yaml
    git commit -m "Add real-time image voting"
    git push -u origin paws-or-claws
    ```

## 4. Validation

1. Run the repository checks for every component you changed. At minimum:

   ```bash
   helm lint ./chart
   skaffold render --digest-source=none --offline \
     --output /tmp/lolcatz.yaml
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     create --dry-run=server -f /tmp/lolcatz.yaml
   ```

   Add voting-service unit tests for input validation, token validation,
   duplicate votes, and aggregate totals.

2. Confirm that the workloads and certificates are ready:

   ```bash
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get pods
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get certificates
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" get ingress
   ```

3. Check the public endpoints. Replace the image ID before running the
   commands:

   ```bash
   export APP_HOST="$(kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     get ingress lolcatz-frontend -o jsonpath='{.spec.rules[0].host}')"
   export IMAGE_ID='<existing-image-id>'

   curl --fail-with-body "https://${APP_HOST}/api/voting/healthz"
   curl --fail-with-body \
     "https://${APP_HOST}/api/voting/images/${IMAGE_ID}/votes"
   ```

4. Verify the HTTP contract with a valid access token:

   ```bash
   export ACCESS_TOKEN='<access-token>'

   curl --fail-with-body \
     -X POST "https://${APP_HOST}/api/voting/images/${IMAGE_ID}/vote" \
     -H "Authorization: Bearer ${ACCESS_TOKEN}" \
     -H 'Content-Type: application/json' \
     --data '{"vote":"up"}'
   ```

   Confirm that invalid input returns `400`, no token returns `401`, the first
   valid vote returns `201`, and a second vote by the same identity returns
   `409`. Send concurrent duplicate requests and confirm that exactly one row is
   created.

5. Open the same image in two authenticated browser sessions. Vote in one and
   confirm that both update without a refresh. Reload both pages and confirm the
   totals remain correct. Open a new session and confirm it immediately receives
   the retained totals.

6. Confirm MQTT authorization: an anonymous client can subscribe to
   `lolcatz/images/+/votes` over `wss://`, but cannot publish. Confirm the browser
   falls back to the HTTP totals endpoint when MQTT is unavailable.

7. Scale the stateless voting service and repeat the two-browser test:

   ```bash
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     scale deployment lolcatz-voting --replicas=2
   kubectl --context "$SKAFFOLD_KUBE_CONTEXT" \
     --namespace "$SKAFFOLD_NAMESPACE" \
     rollout status deployment/lolcatz-voting
   ```

   The exercise is complete when vote uniqueness survives concurrent requests,
   totals survive reloads, live updates work through secure WebSockets, and both
   replicas behave identically.
