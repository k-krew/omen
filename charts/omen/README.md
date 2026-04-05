# Omen Helm Chart

Omen is a lightweight Kubernetes chaos engineering operator.

## Installation

To install the chart from the GitHub Container Registry (GHCR):

```bash
helm install omen oci://ghcr.io/k-krew/charts/omen \
  --version 0.1.0 \
  --namespace omen-system \
  --create-namespace
```

## Configuration

For advanced configuration and usage examples, please refer to the [main repository documentation](https://github.com/k-krew/omen).
