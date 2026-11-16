---
title: Ingress and Gateway
linkTitle: Ingress
weight: 60
---
# Ingress and Gateway

tow installs two ingress controllers itself, because each needs something only tow can do:

- **Traefik** (`ingress.controllers: [traefik]`). k3s brings its own; on kubeadm, tow installs Traefik's chart (42.3.2, Traefik 3.7.17).
  - With a service load balancer, it gets an address from your pool.
  - Without one, it runs on every node and listens on their ports 80 and 443.
  - Its IngressClass is the default only when `defaultClass` says so.
- **Cilium** (`ingress.controllers: [cilium]`, with `cni: cilium`). Its Ingress and Gateway API are settings of Cilium's own install. tow installs Gateway API's definitions (v1.6.3, standard channel) before Cilium, then turns both on. They work through Cilium's kube-proxy replacement and get their addresses from its load balancer, so both are required. You write your own Gateways, with `gatewayClassName: cilium`.

Ports 80 and 443 are opened to the admin addresses (`spec.infrastructure.access.adminCIDRs`, by default your machine) in the AWS security group, the Proxmox firewall and ufw.

## Any other controller

Install it as what it is, a chart or a manifest, under `spec.kubernetes.addons`. tow installs it in order, waits for it, and shows the tool's own error if it fails. If it listens on node ports (no service load balancer), list them in `spec.infrastructure.access.ports` so the firewalls let you in.

These entries are tested together in [`examples/ingress-proxmox.yaml`](../examples/ingress-proxmox.yaml), each on its own MetalLB address. They're ready to copy:

```yaml
spec:
  kubernetes:
    addons:
      helmCharts:
        - name: nginx-ingress                # F5 NGINX Ingress Controller, class "nginx"
          repo: oci://ghcr.io/nginx/charts
          chart: nginx-ingress
          version: 2.8.1
        - name: haproxy                      # HAProxy Kubernetes Ingress, class "haproxy"
          repo: https://haproxytech.github.io/helm-charts
          chart: kubernetes-ingress
          version: 1.54.4
          values: {controller: {service: {type: LoadBalancer}}}   # its default is NodePort
        - name: envoy-gateway                # Gateway API only; brings Gateway API's definitions
          repo: oci://docker.io/envoyproxy
          chart: gateway-helm
          version: v1.10.0
          namespace: envoy-gateway-system
      manifests:
        # Contour has no maintained Helm chart; its own rendered manifest, pinned to a release:
        - {path: "https://raw.githubusercontent.com/projectcontour/contour/v1.33.8/examples/render/contour.yaml", namespace: projectcontour}
        - manifests/ingress-classes.yaml     # IngressClass contour, GatewayClass envoy-gateway
```

Notes:

- **Contour's manifest** always takes ports 80 and 443 on every node (its Envoy DaemonSet), on top of its LoadBalancer Service. No other controller in the cluster can listen on those node ports.
- **Envoy Gateway's chart** creates no GatewayClass. [`examples/manifests/ingress-classes.yaml`](../examples/manifests/ingress-classes.yaml) adds one, and an IngressClass for Contour.
- **Envoy Gateway and Cilium** both bring Gateway API's definitions, so pick one of them for the Gateway API.
- **ingress-nginx** (the Kubernetes community controller) is retired upstream. If you still need it, it's a chart like the others.
