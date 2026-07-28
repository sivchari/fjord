# fjord

fjord runs an EKS-compatible Kubernetes cluster on your local machine, with the same UX as kind.

Manifests written for Amazon EKS apply as-is: the control plane is built from [EKS Distro](https://distro.eks.amazonaws.com/) (the Kubernetes distribution used by Amazon EKS), so the cluster reports an EKS version string and runs the same patched components as a real EKS cluster.

```console
$ fjord create cluster --eks-version 1.33
$ kubectl version --context kind-fjord
Server Version: v1.33.13-eks-...
```

## Why

Developing against EKS locally usually means kind plus a pile of workarounds: excluded manifests, overlay hacks for `gp2`/`gp3` StorageClasses, and "this only works in the real cluster" caveats. fjord removes those diffs at the cluster level instead of patching them in your manifests.

What you get out of the box, matching a new EKS cluster:

- kube-apiserver / kube-controller-manager / kube-scheduler / kube-proxy built by EKS Distro, reporting an `-eks-` version string
- CoreDNS from the EKS Distro build for the selected EKS version
- A `gp2` default StorageClass (backed locally by kind's local-path provisioner)
- IAM integration: IRSA, EKS Pod Identity, IMDS, and access-entry authentication (see below)
- Supported EKS versions 1.29 through 1.36, resolved from a release table that CI keeps in sync with new EKS Distro patch releases

## Usage

```console
# Create a cluster (pulls the prebuilt node image for the requested EKS version)
fjord create cluster --eks-version 1.33

# Delete it
fjord delete cluster

# Build a node image locally from EKS Distro artifacts instead of pulling
fjord create cluster --eks-version 1.33 --build-local

# List supported EKS versions
fjord build node-image --list-versions
```

Node images are published to `ghcr.io/sivchari/fjord/node` and the agent image to `ghcr.io/sivchari/fjord/agent`, both for all supported EKS versions (amd64/arm64).

## IAM integration

fjord emulates the AWS credential and authentication paths an EKS workload relies on, so application SDKs and `kubectl` behave as they would against a real cluster. Credentials are always dummy values — calling real AWS with them fails by design.

**IRSA** — annotate a ServiceAccount and its pods' AWS SDK resolves credentials through the standard web-identity flow:

```console
kubectl annotate serviceaccount my-sa eks.amazonaws.com/role-arn=arn:aws:iam::000000000000:role/my-role
# a pod running as my-sa: aws sts get-caller-identity -> assumed-role/my-role/...
```

**EKS Pod Identity** — associate a ServiceAccount with a role; pods reach credentials through the upstream Pod Identity Agent:

```console
aws eks create-pod-identity-association --endpoint-url http://localhost:48080 \
  --cluster-name fjord --namespace default --service-account my-sa \
  --role-arn arn:aws:iam::000000000000:role/my-role
```

**IMDS** — a bare pod (no annotation) obtains node-role credentials from `169.254.169.254`, the SDK default credential chain's fallback.

**Access-entry authentication** — grant an IAM principal a Kubernetes access policy and use it from `kubectl`, exactly like `aws eks update-kubeconfig` against EKS:

```console
fjord create principal alice
fjord grant access-entry --principal alice --policy View
fjord update-kubeconfig --principal alice
kubectl --context fjord-alice@fjord get pods     # allowed by the View policy
```

The standard access policies map to the built-in Kubernetes roles: `ClusterAdmin` -> `cluster-admin`, `Admin` -> `admin`, `Edit` -> `edit`, `View` -> `view`.

## Development

```console
make test              # unit tests
make lint              # golangci-lint
make test-integration  # end-to-end: builds a node image, creates a real cluster, verifies EKS parity
make generate          # refresh the EKS Distro release table
```

## Scope

fjord aims for behavioral parity — a workload sees the same version strings, default resources, credential paths, and authentication flow as on EKS — not implementation parity. SigV4 signatures are not verified, IAM policy documents are not evaluated, and the emulated credentials never reach real AWS. VPC CNI, Fargate, and any feature requiring real AWS infrastructure are out of scope.

## Disclaimer

fjord is an independent open source project. It is not affiliated with, endorsed by, or sponsored by Amazon Web Services. "Amazon EKS" is a trademark of Amazon.com, Inc. or its affiliates. fjord redistributes EKS Distro artifacts under the terms of the Apache License 2.0.

## License

Apache License 2.0
