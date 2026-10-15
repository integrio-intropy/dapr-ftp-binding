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

Let the component exit when the sidecar does. It watches its own gRPC
socket: daprd connects while loading the component and holds that connection
open for its lifetime, so losing every connection means the sidecar is gone.

```json
"args": ["-exit-when-idle", "30s"]
```

in the `dapr.io/component-container` annotation. The timer only starts once the
sidecar has connected at least once, and any reconnect disarms it, so neither a
slow daprd start nor the connect/disconnect of component discovery will trip it.

Give it a **separate Component** from the one your Deployments use, scoped to
the Job's app-ids. The flag lives on the container, so every pod that injects
that Component inherits it, and in a Deployment an ordinary sidecar restart
would then take the component down with it.
