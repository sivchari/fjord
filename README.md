# fjord

fjord runs an EKS-compatible Kubernetes cluster on your local machine, with the same UX as kind.

Manifests written for Amazon EKS apply as-is: the control plane is built from [EKS Distro](https://distro.eks.amazonaws.com/) (the Kubernetes distribution used by Amazon EKS), so the cluster reports an EKS version string and runs the same patched components as a real EKS cluster.

```console
$ fjord create cluster --eks-version 1.33
$ kubectl version
Server Version: v1.33.5-eks-3025e55
```

## Why

Developing against EKS locally usually means kind plus a pile of workarounds: excluded manifests, overlay hacks for `gp2`/`gp3` StorageClasses, and "this only works in the real cluster" caveats. fjord aims to remove those diffs at the cluster level instead of patching them in your manifests.

## Installation

Prebuilt binaries are available from GitHub releases. Node images are published to `ghcr.io/sivchari/fjord/node` for all supported EKS versions (amd64/arm64).

## Usage

```console
# Create a cluster (pulls the prebuilt node image for the requested EKS version)
fjord create cluster --eks-version 1.33

# Delete it
fjord delete cluster

# Build a node image locally from EKS Distro artifacts (development)
fjord build node-image --eks-version 1.33
```

## Scope

v0 provides EKS-D control plane parity, EKS-style default StorageClass (`gp2`), and EKS-built CoreDNS/kube-proxy. IRSA, EKS Pod Identity, IMDS emulation, and access entries are planned for future releases.

## Disclaimer

fjord is an independent open source project. It is not affiliated with, endorsed by, or sponsored by Amazon Web Services. "Amazon EKS" is a trademark of Amazon.com, Inc. or its affiliates. fjord redistributes EKS Distro artifacts under the terms of the Apache License 2.0.

## License

Apache License 2.0
