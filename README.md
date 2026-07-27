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

Node images are published to `ghcr.io/sivchari/fjord/node` for all supported EKS versions (amd64/arm64).

## Development

```console
make test              # unit tests
make lint              # golangci-lint
make test-integration  # end-to-end: builds a node image, creates a real cluster, verifies EKS parity
make generate          # refresh the EKS Distro release table
```

## Scope

v0 provides EKS-D control plane parity, the EKS default StorageClass, and EKS-built CoreDNS/kube-proxy. IRSA, EKS Pod Identity, IMDS emulation, and access entries are planned for future releases.

## Disclaimer

fjord is an independent open source project. It is not affiliated with, endorsed by, or sponsored by Amazon Web Services. "Amazon EKS" is a trademark of Amazon.com, Inc. or its affiliates. fjord redistributes EKS Distro artifacts under the terms of the Apache License 2.0.

## License

Apache License 2.0
