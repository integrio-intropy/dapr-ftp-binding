# Running the FTP binding on Kubernetes

Automatic container injection needs **Dapr 1.11 or later**. Two things are
required:

1. `dapr.io/inject-pluggable-components: "true"` on the application pod.
2. `dapr.io/component-container` on the Component resource, naming the image.

```bash
kubectl create secret generic ftp-credentials --from-literal=password='...'
kubectl apply -f component.yaml
kubectl apply -f deployment.yaml
```

On Dapr 1.9 and 1.10 the component container has to be added to the pod spec
manually, sharing the `dapr-unix-domain-socket` volume with the sidecar.

To raise the upload limit, pass `-max-message-size` to the component container
and set `dapr.io/max-body-size` on the pod. Download size cannot be raised; see
the main README.

## Jobs and CronJobs

A pod is only `Succeeded` once **every** container has terminated, and the Dapr
injector adds pluggable components to `spec.containers`. So a Job whose app
finishes its work and shuts the sidecar down still has this component running,
and the Job never completes.

Let the component exit when the sidecar does. It polls daprd's own HTTP port
on localhost, which every container in the pod shares and which daprd answers
on for as long as it runs.

This is deliberately *not* watched via our own gRPC socket. That looks like a
usable signal — daprd connects while loading the component — but grpc-go's
client-side connection idling closes daprd's connection to us after about 30
minutes without an Invoke call, then reconnects transparently on the next one.
That is routine and harmless, but it looks identical to "the sidecar is gone"
to anything watching the socket, so a component that trusted it would exit out
from under a sidecar that is still very much alive and about to call it again
— typically about half an hour into an otherwise healthy Job.

```json
"args": ["-exit-when-idle", "30s"]
```

in the `dapr.io/component-container` annotation. Once daprd's HTTP port has
answered, any later answer disarms the timer, so a brief blip will not trip
it.

A sidecar that is never seen at all is handled by the same poll, via
`-exit-when-no-sidecar` (default `1m`). If daprd dies before reaching this
container — or scans the socket folder before our socket exists and gives up
with `couldn't find binding` — its HTTP port never answers either, and without
a bound the component would sit there until someone kills it. The startup wait
bounds that. It has to be generous — a slow image pull or a sidecar loading
many components is an honest start, not a dead one — so raise it if your
sidecars routinely take longer than five minutes to reach the component, and
set it to `0` to keep waiting indefinitely:

```json
"args": ["-exit-when-idle", "30s", "-exit-when-no-sidecar", "10m"]
```

Exits from either timer are exit code 0, so the pod can reach `Succeeded`. If
the sidecar dying on startup should instead fail the Job, give the Job an
`activeDeadlineSeconds` or have the app container exit non-zero when it cannot
reach Dapr; this flag deliberately does not turn a missing sidecar into a
failure, because it cannot tell one from a sidecar that was simply never going
to arrive on this pod.

`-exit-when-idle` lives on the container, so every pod that injects the
Component inherits it — and in a Deployment an ordinary sidecar restart would
then take the component down with it. Two ways to avoid that:

- **Give Jobs a separate Component**, scoped to their app-ids with `scopes`,
  and leave `-exit-when-idle` off the one your Deployments use.
- **Share one Component**, and let each pod opt out with the
  `FTP_BINDING_EXIT_WHEN_IDLE` env var. Bake `-exit-when-idle 30s` into the
  Component's args as the default for Jobs and CronJobs, then map a pod
  annotation to that env var via the Kubernetes Downward API so a Deployment's
  pod can turn it off:

  ```json
  "env": [{
    "name": "FTP_BINDING_EXIT_WHEN_IDLE",
    "valueFrom": { "fieldRef": { "fieldPath": "metadata.annotations['ftp-binding.intropy.io/exit-when-idle']" } }
  }]
  ```

  ```yaml
  annotations:
    ftp-binding.intropy.io/exit-when-idle: "off"
  ```

  A pod without the annotation (a Job) falls through to the Component's own
  `-exit-when-idle`; one with it set to `off` disables idle-exit for that pod
  alone. See `component.yaml` and `deployment.yaml` for the full wiring.
