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

Run the binding and daprd as Kubernetes native sidecars: init containers with
`restartPolicy: Always`. [`cronjob.yaml`](cronjob.yaml) is a complete example.

```bash
kubectl apply -f component.yaml
kubectl apply -f cronjob.yaml
```

What the pod needs:

1. The binding declared under `initContainers` with `restartPolicy: Always`
   and a startup probe on its socket.
2. `dapr.io/enable-native-sidecar: "true"`, so Dapr injects daprd as a native
   sidecar. The injector appends daprd to `initContainers`, after the binding.
3. `dapr.io/inject-pluggable-components: "false"`, because the pod brings the
   binding itself.
4. An `emptyDir` volume for the socket folder, mounted in the binding and
   given to daprd with `dapr.io/volume-mounts`.

The kubelet then does the rest:

- **Start order.** daprd starts only when the binding's startup probe has
  passed, so it finds the socket on its first start. daprd reads the socket
  folder once, and an injected binding usually loses that race: daprd exits
  with `couldn't find binding` and has to be restarted.
- **Completion.** When the app container exits, the kubelet stops daprd and
  the binding. The pod's result is the app's exit code. The app does not need
  to call `/v1.0/shutdown`.
- **Restarts.** A native sidecar is restarted whatever the pod's
  `restartPolicy`, so `Never` is fine for the Job.

Needs Kubernetes 1.29 or later. The image and arguments now live in the
workload instead of on the Component; keep them equal to the
`dapr.io/component-container` annotation if Deployments use that.

### Do not inject the binding into a Job

With `dapr.io/inject-pluggable-components: "true"` the Dapr injector adds the
binding to `spec.containers`. A pod is only `Succeeded` once **every**
container has terminated, and nothing stops that container when the app is
done, so the Job never completes. Earlier versions of the binding could exit
by themselves for this case (`-exit-when-idle`); that was removed in favour of
the native sidecar layout above.
