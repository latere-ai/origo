# Bootstrap resources

The release pipeline never reads this directory. Everything it applies must
be idempotent, because it applies on every release, and the resources here
are one-time: the namespace and the secrets.

```sh
kubectl apply -f deploy/bootstrap/namespace.yaml
cp deploy/bootstrap/secrets.example.yaml /tmp/origod-secrets.yaml
# fill in the bucket credentials, the issuers, the authorizer, and the
# gossip secret, then
kubectl apply -f /tmp/origod-secrets.yaml
```

The third Secret, `origod-token-key`, is not a template: it holds the
key that signs repository-bound tokens, which must be the operator's
own. Step 4 of [`../../docs/install.md`](../../docs/install.md)
generates it, and the Deployment reads it by name.

A resource that belongs to a release belongs in `deploy/base/` with the
target overlay under `deploy/prod/`, not here.
