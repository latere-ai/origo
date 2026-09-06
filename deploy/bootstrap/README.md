# Bootstrap resources

The release pipeline never reads this directory. Everything it applies must
be idempotent, because it applies on every release, and the resources here
are one-time: the namespace and the secrets.

```sh
kubectl apply -f deploy/bootstrap/namespace.yaml
cp deploy/bootstrap/secrets.example.yaml /tmp/origod-secrets.yaml
# fill in the bucket credentials and the phase 1 bearer, then
kubectl apply -f /tmp/origod-secrets.yaml
```

A resource that belongs to a release belongs in `deploy/base/` with the
target overlay under `deploy/prod/`, not here.
