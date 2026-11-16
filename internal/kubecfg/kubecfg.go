// Package kubecfg handles the kubeconfig a cluster hands out: tow keeps a copy in the
// cluster's state folder and, unless the file says otherwise, merges it into the user's
// kubeconfig (~/.kube/config, or the files $KUBECONFIG lists) under the context name the
// file chose. A kubeconfig holds the cluster's admin key: never print or log one.
// (Not named "kubeconfig": .gitignore keeps every path with that name out of git.)
package kubecfg

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Rename gives the cluster, user and context of a single-cluster kubeconfig one name, and
// makes that context current.
func Rename(data []byte, name string) ([]byte, error) {
	in, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	if len(in.Contexts) != 1 {
		return nil, fmt.Errorf("expected a kubeconfig with one context, found %d", len(in.Contexts))
	}
	var ctx *api.Context
	for _, c := range in.Contexts {
		ctx = c
	}
	cluster, user := in.Clusters[ctx.Cluster], in.AuthInfos[ctx.AuthInfo]
	if cluster == nil || user == nil {
		return nil, fmt.Errorf("kubeconfig context points to a missing cluster or user")
	}

	out := api.NewConfig()
	out.Clusters[name] = cluster
	out.AuthInfos[name] = user
	out.Contexts[name] = &api.Context{Cluster: name, AuthInfo: name, Namespace: ctx.Namespace}
	out.CurrentContext = name
	return clientcmd.Write(*out)
}

// Point sets the API address of every cluster in a kubeconfig: k3s writes its own as
// https://127.0.0.1:6443, which only works on the machine itself.
func Point(data []byte, server string) ([]byte, error) {
	in, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	for _, c := range in.Clusters {
		c.Server = server
	}
	return clientcmd.Write(*in)
}

// WithNamespace reads the kubeconfig at path and returns it with its current context's
// namespace set: kubectl then puts objects that name no namespace there, and leaves alone
// those that name one (its --namespace flag refuses those).
func WithNamespace(path, namespace string) ([]byte, error) {
	in, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	ctx := in.Contexts[in.CurrentContext]
	if ctx == nil {
		return nil, fmt.Errorf("kubeconfig %s has no current context", path)
	}
	ctx.Namespace = namespace
	return clientcmd.Write(*in)
}

// Store keeps a cluster's kubeconfig at path under the context name, and merges it into the
// user's kubeconfig (switching to it) unless merge is false.
func Store(data []byte, context, path string, merge bool) error {
	data, err := Rename(data, context)
	if err != nil {
		return err
	}
	if err := Save(path, data); err != nil {
		return err
	}
	if merge {
		return Merge(data)
	}
	return nil
}

// CheckFree refuses a name the user's kubeconfig already uses: the cluster's context, cluster
// and user all get it (Rename), so merging would replace an entry another context may use.
func CheckFree(name string) error {
	cfg, err := clientcmd.NewDefaultPathOptions().GetStartingConfig()
	if err != nil {
		return fmt.Errorf("read your kubeconfig: %w", err)
	}
	if _, ok := cfg.Contexts[name]; ok {
		return fmt.Errorf("your kubeconfig already has a context named '%s'; set spec.kubernetes.kubeconfig.context to another name, or remove it ('kubectl config delete-context %s')", name, name)
	}
	if _, ok := cfg.Clusters[name]; ok {
		return fmt.Errorf("your kubeconfig already has a cluster named '%s' (another context may use it); set spec.kubernetes.kubeconfig.context to another name, or remove it ('kubectl config delete-cluster %s')", name, name)
	}
	if _, ok := cfg.AuthInfos[name]; ok {
		return fmt.Errorf("your kubeconfig already has a user named '%s' (another context may use it); set spec.kubernetes.kubeconfig.context to another name, or remove it ('kubectl config delete-user %s')", name, name)
	}
	return nil
}

// Save writes a kubeconfig only its owner can read, in a folder only its owner can open.
func Save(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// HasContext reports whether the user's kubeconfig already has a context with this name.
func HasContext(name string) (bool, error) {
	cfg, err := clientcmd.NewDefaultPathOptions().GetStartingConfig()
	if err != nil {
		return false, fmt.Errorf("read your kubeconfig: %w", err)
	}
	_, ok := cfg.Contexts[name]
	return ok, nil
}

// Merge adds a kubeconfig's entries to the user's kubeconfig and makes its context current,
// as kubectl would: entries go to the file that already has them, new ones to the default.
func Merge(data []byte) error { return merge(data, func(string) bool { return true }) }

// Add merges like Merge, but leaves the user's current context alone, unless it is replaces:
// the same cluster under its old context name.
func Add(data []byte, replaces string) error {
	return merge(data, func(current string) bool { return current == replaces })
}

func merge(data []byte, switchFrom func(current string) bool) error {
	in, err := clientcmd.Load(data)
	if err != nil {
		return fmt.Errorf("read kubeconfig: %w", err)
	}
	po := clientcmd.NewDefaultPathOptions()
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return fmt.Errorf("read your kubeconfig: %w", err)
	}
	maps.Copy(cfg.Clusters, in.Clusters)
	maps.Copy(cfg.AuthInfos, in.AuthInfos)
	maps.Copy(cfg.Contexts, in.Contexts)
	if switchFrom(cfg.CurrentContext) {
		cfg.CurrentContext = in.CurrentContext
	}
	if err := clientcmd.ModifyConfig(po, *cfg, true); err != nil {
		return fmt.Errorf("update your kubeconfig: %w", err)
	}
	return nil
}

// Remove deletes a context from the user's kubeconfig, with its cluster and user when no
// other context uses them. A context that isn't there is not an error.
func Remove(name string) error {
	po := clientcmd.NewDefaultPathOptions()
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return fmt.Errorf("read your kubeconfig: %w", err)
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return nil
	}
	delete(cfg.Contexts, name)
	clusterUsed, userUsed := false, false
	for _, c := range cfg.Contexts {
		clusterUsed = clusterUsed || c.Cluster == ctx.Cluster
		userUsed = userUsed || c.AuthInfo == ctx.AuthInfo
	}
	if !clusterUsed {
		delete(cfg.Clusters, ctx.Cluster)
	}
	if !userUsed {
		delete(cfg.AuthInfos, ctx.AuthInfo)
	}
	if cfg.CurrentContext == name {
		cfg.CurrentContext = ""
	}
	if err := clientcmd.ModifyConfig(po, *cfg, true); err != nil {
		return fmt.Errorf("update your kubeconfig: %w", err)
	}
	return nil
}
