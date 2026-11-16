package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// echo is a throwaway namespace with an agnhost netexec Deployment behind a ClusterIP Service
// on port 80; remove deletes the namespace.
func echo(t *testing.T, ctx context.Context, cs kubernetes.Interface, ns string) (remove func()) {
	t.Helper()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "echo"}
	replicas := int32(1)
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "echo"}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "netexec", Image: agnhost, Args: []string{"netexec", "--http-port=8080"}}}}},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "echo"}, Spec: corev1.ServiceSpec{
		Selector: labels, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	}
}

// get calls http://addr/hostname with a Host header from this machine; the answer is the
// echo pod's name.
func get(addr, host string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/hostname", nil)
	if err != nil {
		return "", err
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// checkIngress proves every IngressClass in the cluster serves an Ingress from this machine:
// built-in Traefik and Cilium, and the controllers a file installs as charts. The address is
// the one the controller publishes in the Ingress's status; built-in Traefik without a
// service load balancer is on every node's port 80.
func checkIngress(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config, nodes []providers.Node) {
	t.Helper()
	classes, err := cs.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil || len(classes.Items) == 0 {
		return
	}
	defer echo(t, ctx, cs, "tow-check-ingress")()
	ns := "tow-check-ingress"
	prefix := networkingv1.PathTypePrefix
	for _, class := range classes.Items {
		host := class.Name + ".tow.test"
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: class.Name}, Spec: networkingv1.IngressSpec{
			IngressClassName: &class.Name,
			Rules: []networkingv1.IngressRule{{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{Name: "echo", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}},
		}}
		if _, err := cs.NetworkingV1().Ingresses(ns).Create(ctx, ing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		// Traefik on the nodes' ports: its DaemonSet without a load balancer, or behind k3s's
		// servicelb, which publishes the nodes' private addresses (unreachable from here on a
		// cloud): a worker's own address is called instead.
		onNodes := class.Name == config.IngressTraefik && !addons.ServiceLBInstalled(cfg.Spec.Kubernetes.ServiceLoadBalancer.Provider)
		var addr, last string
		eventuallyWithin(t, ctx, 5*time.Minute, "IngressClass "+class.Name+" serving "+host+" from this machine", func() bool {
			if onNodes {
				addr = net.JoinHostPort(nodes[len(nodes)-1].Host, "80") // a worker, where its DaemonSet runs
			} else if got, err := cs.NetworkingV1().Ingresses(ns).Get(ctx, class.Name, metav1.GetOptions{}); err == nil && len(got.Status.LoadBalancer.Ingress) > 0 {
				addr = got.Status.LoadBalancer.Ingress[0].IP
			}
			if addr == "" {
				return false
			}
			out, err := get(addr, host)
			if err != nil {
				last = err.Error()
				return false
			}
			last = out
			return strings.HasPrefix(out, "echo-")
		})
		t.Logf("IngressClass %s at %s answered: %s", class.Name, addr, last)
	}
}

// checkGateway proves every accepted GatewayClass (Cilium's, Envoy Gateway's) serves a
// Gateway and an HTTPRoute from this machine, on the address the Gateway publishes.
func checkGateway(t *testing.T, ctx context.Context, cs kubernetes.Interface) {
	t.Helper()
	rest := cs.CoreV1().RESTClient()
	raw, err := rest.Get().AbsPath("/apis/gateway.networking.k8s.io/v1/gatewayclasses").DoRaw(ctx)
	if err != nil {
		return // no Gateway API in this cluster
	}
	var classes struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &classes) != nil || len(classes.Items) == 0 {
		return
	}
	ns := "tow-check-gateway"
	defer echo(t, ctx, cs, ns)()
	for _, c := range classes.Items {
		name := c.Metadata.Name
		host := name + ".gateway.tow.test"
		gw := fmt.Sprintf(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":%q},
"spec":{"gatewayClassName":%q,"listeners":[{"name":"http","protocol":"HTTP","port":80}]}}`, name, name)
		route := fmt.Sprintf(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute","metadata":{"name":%q},
"spec":{"parentRefs":[{"name":%q}],"hostnames":[%q],"rules":[{"backendRefs":[{"name":"echo","port":80}]}]}}`, name, name, host)
		for kind, body := range map[string]string{"gateways": gw, "httproutes": route} {
			if _, err := rest.Post().AbsPath("/apis/gateway.networking.k8s.io/v1/namespaces/" + ns + "/" + kind).Body([]byte(body)).DoRaw(ctx); err != nil {
				t.Fatalf("create %s %s: %v", kind, name, err)
			}
		}
		var addr, last string
		eventuallyWithin(t, ctx, 5*time.Minute, "GatewayClass "+name+" serving "+host+" from this machine", func() bool {
			raw, err := rest.Get().AbsPath("/apis/gateway.networking.k8s.io/v1/namespaces/" + ns + "/gateways/" + name).DoRaw(ctx)
			var g struct {
				Status struct {
					Addresses []struct{ Value string } `json:"addresses"`
				} `json:"status"`
			}
			// Each new reason it isn't served yet is logged: a timeout then says what it last saw.
			note := func(why string) bool {
				if why != last {
					t.Logf("GatewayClass %s not serving yet: %s", name, why)
				}
				last = why
				return false
			}
			if err != nil || json.Unmarshal(raw, &g) != nil {
				return note(fmt.Sprintf("read the Gateway: %v", err))
			}
			if len(g.Status.Addresses) == 0 {
				return note("the Gateway has no address yet")
			}
			addr = g.Status.Addresses[0].Value
			out, err := get(net.JoinHostPort(addr, "80"), host)
			if err != nil {
				return note(addr + ": " + err.Error())
			}
			last = out
			return strings.HasPrefix(out, "echo-")
		})
		t.Logf("GatewayClass %s at %s answered: %s", name, addr, last)
	}
}

// checkAccessPorts proves the machines' firewalls let this machine in on the file's
// access.ports: a pod listening there on a node's port answers at the node's address.
func checkAccessPorts(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config, nodes []providers.Node) {
	t.Helper()
	ports := cfg.Spec.Infrastructure.Access.Ports
	if len(ports) == 0 {
		return
	}
	ns := "tow-check-ports"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	}()
	for _, port := range ports {
		name := "port-" + strconv.Itoa(port)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "netexec", Image: agnhost, Args: []string{"netexec", "--http-port=" + strconv.Itoa(port)},
			Ports: []corev1.ContainerPort{{ContainerPort: int32(port), HostPort: int32(port)}},
		}}}}
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		var host, last string
		eventually(t, ctx, fmt.Sprintf("port %d (access.ports) answering from this machine", port), func() bool {
			p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil || p.Status.Phase != corev1.PodRunning {
				return false
			}
			i := slices.IndexFunc(nodes, func(n providers.Node) bool { return n.Name == p.Spec.NodeName })
			if i < 0 {
				return false
			}
			host = nodes[i].Host
			out, err := get(net.JoinHostPort(host, strconv.Itoa(port)), "x")
			last = out
			if err != nil {
				last = err.Error()
			}
			return err == nil && out == name
		})
		t.Logf("port %d on %s answered: %s", port, host, last)
	}
}
