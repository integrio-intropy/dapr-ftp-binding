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
