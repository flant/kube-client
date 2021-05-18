# Kube-client

A kubernetes client wrapped with metrics storage and automatic config setup.

## Metrics

* `{PREFIX}_kubernetes_client_request_result_total` — a counter of requests made by kubernetes/client-go library.
* `{PREFIX}_kubernetes_client_request_latency_seconds` — a histogram with latency of requests made by
  kubernetes/client-go library.

# Community

Please feel free to reach developers/maintainers and users
via [GitHub Discussions](https://github.com/flant/shell-operator/discussions) for any questions regarding
shell-operator.

You're also welcome to follow [@flant_com](https://twitter.com/flant_com) to stay informed about all our Open Source
initiatives.

# License

Apache License 2.0, see [LICENSE](LICENSE).
