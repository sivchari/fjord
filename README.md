# fjord

fjord runs an EKS-compatible Kubernetes cluster on your local machine, built on [rask](https://github.com/sivchari/rask).

Manifests written for Amazon EKS apply as-is: on Linux, the control plane is built from [EKS Distro](https://distro.eks.amazonaws.com/) (the Kubernetes distribution used by Amazon EKS), so the cluster reports an EKS version string and runs the same patched components as a real EKS cluster. On macOS, rask's vz substrate does not yet support EKS Distro component overrides, so the control plane runs upstream Kubernetes instead; fjord's AWS emulation layer (IRSA, IMDS, EKS Pod Identity, access-entry authentication) still works there.

```console
$ fjord create cluster --eks-version 1.33
$ kubectl version --context fjord
Server Version: v1.33.13-eks-...
```

## Why

Developing against EKS locally usually means a pile of workarounds: excluded manifests, overlay hacks for `gp2`/`gp3` StorageClasses, and "this only works in the real cluster" caveats. fjord removes those diffs at the cluster level instead of patching them in your manifests.

What you get out of the box, matching a new EKS cluster (Linux):

- kube-apiserver / kube-controller-manager / kube-scheduler / kube-proxy built by EKS Distro, reporting an `-eks-` version string
- CoreDNS from the EKS Distro build for the selected EKS version
- `gp2` (default) and `gp3` StorageClasses (backed locally by rask's bundled local-path provisioner) so EKS PVCs bind unchanged
- IAM integration: IRSA, EKS Pod Identity, IMDS, and access-entry authentication (see below)
- NetworkPolicy objects are actually enforced (via bundled [kube-network-policies](https://github.com/kubernetes-sigs/kube-network-policies)), matching an EKS cluster with the VPC CNI's network policy support enabled
- Supported EKS versions 1.29 through 1.36, resolved from a release table that CI keeps in sync with new EKS Distro patch releases

## Usage

```console
# Create a cluster (materializes EKS Distro components for the requested version, on Linux)
fjord create cluster --eks-version 1.33

# Delete it
fjord delete cluster

# Build and load the fjord-agent image locally instead of pulling the published one
fjord create cluster --eks-version 1.33 --build-local
```

Clusters are named. `--name` defaults to `fjord`, and the name is also the kubectl
context, which `create` merges into your default kubeconfig and makes current
(`delete` removes it again). Several clusters can coexist:

```console
fjord create cluster --name alpha
fjord create cluster --name beta
kubectl --context alpha get nodes
fjord delete cluster --name alpha
```

The fjord-agent image is published to `ghcr.io/sivchari/fjord/agent` for all supported EKS versions (amd64/arm64).

## Running fjord inside a container

fjord runs the control plane as host processes in whatever network namespace it is
given, so it works inside a container — including a pod on another Kubernetes
cluster — as long as the container can act like a node. That needs
`privileged: true`: capabilities alone are not enough, because `/dev/kmsg` is
absent and `/sys/fs/cgroup` is read-only without it.

Four things bite in that setup, in the order you hit them:

1. **Put the data directory on a real filesystem.** fjord's state lives under
   `$HOME/.rask`. If that is on the container's overlayfs rootfs, the nested
   containerd cannot mount its own overlay and every pod sandbox fails with
   `failed to mount rootfs component ... invalid argument`. Point `HOME` at a
   volume mount instead.

2. **Isolate the cgroup namespace before touching cgroups.** A privileged pod
   sees the *host's* cgroup namespace, so `/sys/fs/cgroup` is the node's root
   cgroup and is writable. Preparing cgroups there rewrites the placement of
   every process on the node. Do it inside `unshare --cgroup --mount`, make `/`
   rprivate, remount `/sys/fs/cgroup`, then move your own processes into a child
   cgroup so `cgroup.subtree_control` becomes writable.

3. **Drop the outer cluster's environment.** Kubernetes injects
   `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` into every pod. The
   nested kube-controller-manager falls back to in-cluster config, talks to the
   *outer* apiserver, and fails to start with
   `configmaps "extension-apiserver-authentication" is forbidden`. Unset them.

4. **Make `br_netfilter` loadable.** Without it, pod-to-pod traffic across the
   bridge skips netfilter, so kube-proxy and NetworkPolicy are silently bypassed.
   fjord loads it at startup, but `modprobe` reads the *container's*
   `/lib/modules`, so mount the node's module tree read-only if the module is not
   already loaded.

## IAM integration

fjord emulates the AWS credential and authentication paths an EKS workload relies on, so application SDKs and `kubectl` behave as they would against a real cluster. Credentials are always dummy values — calling real AWS with them fails by design.

**IRSA** — annotate a ServiceAccount and its pods' AWS SDK resolves credentials through the standard web-identity flow:

```console
kubectl annotate serviceaccount my-sa eks.amazonaws.com/role-arn=arn:aws:iam::000000000000:role/my-role
# a pod running as my-sa: aws sts get-caller-identity -> assumed-role/my-role/...
```

**EKS Pod Identity** — associate a ServiceAccount with a role; pods reach credentials through the upstream Pod Identity Agent:

```console
aws eks create-pod-identity-association --endpoint-url http://localhost:30080 \
  --cluster-name fjord --namespace default --service-account my-sa \
  --role-arn arn:aws:iam::000000000000:role/my-role
```

**IMDS** — a bare pod (no annotation) obtains node-role credentials from `169.254.169.254`, the SDK default credential chain's fallback.

**Working AWS services via kumo** — by default fjord deploys [kumo](https://github.com/sivchari/kumo) (a local AWS emulator) into the cluster and injects `AWS_ENDPOINT_URL` into IAM-identity pods, so their non-STS AWS calls (S3, Secrets Manager, SQS, ...) actually resolve against kumo rather than only passing `kubectl apply`. STS stays on fjord because the SDK prefers the service-specific `AWS_ENDPOINT_URL_STS`. This lets an External Secrets Operator `ClusterSecretStore` pointed at Secrets Manager resolve real values locally. Disable with `--with-kumo=false`, or point at an external emulator with `--aws-endpoint-url`.

```console
# inside an IAM-identity pod: aws secretsmanager get-secret-value / aws s3 ... hit kumo
```

**Access-entry authentication** — grant an IAM principal a Kubernetes access policy and use it from `kubectl`, exactly like `aws eks update-kubeconfig` against EKS:

```console
fjord create principal alice
fjord grant access-entry --principal alice --policy View
fjord update-kubeconfig --principal alice
kubectl --context fjord-alice@fjord get pods     # allowed by the View policy
```

The standard access policies map to the built-in Kubernetes roles: `ClusterAdmin` -> `cluster-admin`, `Admin` -> `admin`, `Edit` -> `edit`, `View` -> `view`.

## EKS API facade

fjord answers the read side of the EKS API, so `aws eks`, `eksctl`, and terraform's `aws_eks_cluster` data source can inspect it like a real cluster:

```console
aws eks list-clusters   --endpoint-url http://localhost:30080
aws eks describe-cluster --name fjord --endpoint-url http://localhost:30080   # version, oidc issuer, networkConfig, accessConfig, ...
aws eks list-addons     --cluster-name fjord --endpoint-url http://localhost:30080   # coredns, kube-proxy, eks-pod-identity-agent
```

`describe-addon` reports only the addons fjord actually runs; asking for one it does not (e.g. `vpc-cni`, since fjord's rask substrate provides its own pod networking) returns `ResourceNotFoundException`, matching EKS.

## LoadBalancer services

`--with-loadbalancer` (requires `--enable-auth`) starts fjord-agent's own LoadBalancer controller, which claims every class-less `type: LoadBalancer` Service with no external IP yet by publishing the cluster's node address(es) to it, since rask's hostproc runtime routes NodePort traffic through kube-proxy on the host.

## Development

```console
make test              # unit tests
make lint              # golangci-lint
make test-integration  # end-to-end: creates a real cluster, verifies EKS parity
make generate          # refresh the EKS Distro release table
```

## Scope

fjord aims for behavioral parity — a workload sees the same version strings, default resources, credential paths, and authentication flow as on EKS — not implementation parity. SigV4 signatures are not verified, IAM policy documents are not evaluated, and the emulated credentials never reach real AWS. VPC CNI, Fargate, and any feature requiring real AWS infrastructure are out of scope.

## Disclaimer

fjord is an independent open source project. It is not affiliated with, endorsed by, or sponsored by Amazon Web Services. "Amazon EKS" is a trademark of Amazon.com, Inc. or its affiliates. fjord redistributes EKS Distro artifacts under the terms of the Apache License 2.0.

## License

Apache License 2.0
