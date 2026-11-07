# Deploy

Kustomize manifests for an installation of `origod`, the example overlays
an installation starts from, and the maintainer's own installation.
[`docs/install.md`](../docs/install.md) walks an installation from an
empty cluster to a serving one; start there.

| Directory | Holds |
|---|---|
| `base/` | what `origod` needs in any target: the Deployment, Service, ServiceAccount, NetworkPolicy, Ingress, PodDisruptionBudget and HorizontalPodAutoscaler, and a PrometheusRule beside them that the base does not list, because it needs the Prometheus operator. The base is not applied directly; an overlay supplies the namespace |
| `bootstrap/` | what is applied once by hand and never by a release: the Namespace, the Secret template for the object store and the identity settings, and the identity a pipeline deploys with. The signing key and the SSH host keys are Secrets that `docs/install.md` has you generate. Its [README](bootstrap/README.md) gives the order |
| `examples/` | overlays of the base to copy and edit: `kind/` is a laptop cluster with an object store and stub identity services, `aws/` is an installation on EKS, and `digitalocean/` is one on DigitalOcean Kubernetes. Each names the files to change in its header |
| `prod/` | the maintainer's own installation, not an install path: the base with the maintainer's ingresses, public address, authorizer, SSH key lookup, push-event sink, replica counts and resource requests. A release applies it through the `deploy` job of `.github/workflows/release.yml` only where the repository variable `ORIGO_RELEASE_DEPLOY` is set, so a fork deploys nothing. It holds only manifests that are safe to apply again |

The release attaches a deploy archive, `deploy-<tag>.tar.gz`, that
`tools/release/deploy-archive.sh` packs from `base/` and `examples/` with
the images pinned to the tag, together with `docs/install.md`. It does not
copy `bootstrap/` or `prod/`.
