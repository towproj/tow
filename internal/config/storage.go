package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Storage drivers (CSI). Several can coexist; each one's StorageClass is named after it.
const (
	CSILocalPath = "local-path"
	CSIOpenEBS   = "openebs"
	CSILonghorn  = "longhorn"
	CSINFS       = "nfs"
	CSISMB       = "smb"
	CSIRookCeph  = "rook-ceph"
	CSIAWSEBS    = "aws-ebs"
	CSIAWSEFS    = "aws-efs"
	CSIProxmox   = "proxmox"
	CSIGCPPD     = "gcp-pd"
	CSIAzureDisk = "azure-disk"
)

type Storage struct {
	CSI          []string  `yaml:"csi,omitempty"`
	DefaultClass string    `yaml:"defaultClass,omitempty"` // one of csi; default: the only one listed
	LocalPath    *Pinned   `yaml:"local-path,omitempty"`
	OpenEBS      *Pinned   `yaml:"openebs,omitempty"`
	Longhorn     *Longhorn `yaml:"longhorn,omitempty"`
	NFS          *NFS      `yaml:"nfs,omitempty"`
	SMB          *SMB      `yaml:"smb,omitempty"`
	RookCeph     *Pinned   `yaml:"rook-ceph,omitempty"`
	AWSEBS       *Pinned   `yaml:"aws-ebs,omitempty"`
	AWSEFS       *Pinned   `yaml:"aws-efs,omitempty"`
	Proxmox      *Pinned   `yaml:"proxmox,omitempty"`
	GCPPD        *Pinned   `yaml:"gcp-pd,omitempty"` // the driver's release; its manifests are the tested one's
	AzureDisk    *Pinned   `yaml:"azure-disk,omitempty"`
}

// ProxmoxCSITokenEnv holds the Proxmox CSI plugin's own token (id=secret): a user with only
// the plugin's role, never the admin token tow creates VMs with (every pod that can read
// the plugin's Secret would hold it).
const ProxmoxCSITokenEnv = "PROXMOX_CSI_TOKEN"

// Pinned is the options of a component that takes only a version (so far).
type Pinned struct {
	Version string `yaml:"version,omitempty"` // empty = the version tow was tested with
}

type Longhorn struct {
	Version  string `yaml:"version,omitempty"`
	Replicas *int   `yaml:"replicas,omitempty"` // copies of each volume; empty = Longhorn's default
}

type NFS struct {
	Version string `yaml:"version,omitempty"`
	Server  string `yaml:"server,omitempty"` // the NFS server's address
	Path    string `yaml:"path,omitempty"`   // the exported directory
}

// SMB credentials come from environment variables the file names, never from the file.
type SMB struct {
	Version     string `yaml:"version,omitempty"`
	Source      string `yaml:"source,omitempty"` // //server/share
	UsernameEnv string `yaml:"usernameEnv,omitempty"`
	PasswordEnv string `yaml:"passwordEnv,omitempty"`
}

var storageChoices = map[string]choice{
	CSILocalPath: {},
	CSIOpenEBS:   {distributions: remote},
	CSILonghorn:  {distributions: remote},
	CSINFS:       {distributions: remote},
	CSISMB:       {distributions: remote},
	CSIRookCeph:  {distributions: remote},
	CSIAWSEBS:    {distributions: remote, providers: []string{ProviderAWS}, why: "it uses AWS volumes"},
	CSIAWSEFS:    {distributions: remote, providers: []string{ProviderAWS}, why: "it uses AWS file systems"},
	CSIProxmox:   {distributions: remote, providers: []string{ProviderProxmox}, why: "it uses Proxmox disks"},
	CSIGCPPD:     {distributions: remote, providers: []string{ProviderGCP}, why: "it uses Compute Engine persistent disks"},
	CSIAzureDisk: {distributions: remote, providers: []string{ProviderAzure}, why: "it uses Azure managed disks"},
}

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	smbSource = regexp.MustCompile(`^//[^/\s]+/\S+$`)
)

func (c *Config) validateStorage() []error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	addf := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.kubernetes.storage"
	s := c.Spec.Kubernetes.Storage

	selected := map[string]bool{}
	for i, name := range s.CSI {
		path := fmt.Sprintf("%s.csi[%d]", at, i)
		if selected[name] {
			addf("%s: %s is listed twice", path, name)
			continue
		}
		selected[name] = true
		add(c.checkChoice(path, name, storageChoices))
	}
	errs = append(errs, checkBlocks(at, []optionBlock{
		{CSILocalPath, s.LocalPath != nil}, {CSIOpenEBS, s.OpenEBS != nil}, {CSILonghorn, s.Longhorn != nil},
		{CSINFS, s.NFS != nil}, {CSISMB, s.SMB != nil}, {CSIRookCeph, s.RookCeph != nil},
		{CSIAWSEBS, s.AWSEBS != nil}, {CSIAWSEFS, s.AWSEFS != nil}, {CSIProxmox, s.Proxmox != nil},
		{CSIGCPPD, s.GCPPD != nil}, {CSIAzureDisk, s.AzureDisk != nil},
	}, func(name string) bool { return selected[name] }, "storage.csi doesn't list it")...)

	switch {
	case s.DefaultClass != "" && !selected[s.DefaultClass]:
		addf("%s.defaultClass %q must be one of storage.csi", at, s.DefaultClass)
	case s.DefaultClass != "" && s.DefaultClass != CSILocalPath && selected[CSILocalPath] && c.bundles():
		addf("%s.defaultClass %s: k3s marks its bundled local-path as the default StorageClass, and marks it again each time it starts; make local-path the default, or leave it out of storage.csi", at, s.DefaultClass)
	}
	if len(s.CSI) > 1 && s.DefaultClass == "" {
		c.warnings = append(c.warnings, "no default StorageClass: volume claims that name no class stay Pending\n  → set spec.kubernetes.storage.defaultClass")
	}

	// Options of the selected drivers only; the others were refused above.
	pinned := map[string]*Pinned{CSILocalPath: s.LocalPath, CSIOpenEBS: s.OpenEBS, CSIRookCeph: s.RookCeph, CSIAWSEBS: s.AWSEBS, CSIAWSEFS: s.AWSEFS, CSIProxmox: s.Proxmox, CSIGCPPD: s.GCPPD, CSIAzureDisk: s.AzureDisk}
	for _, name := range sortedKeys(pinned) {
		if p := pinned[name]; p != nil && selected[name] {
			add(c.checkVersion(at+"."+name+".version", name, p.Version))
		}
	}
	if l := s.Longhorn; l != nil && selected[CSILonghorn] {
		add(c.checkVersion(at+".longhorn.version", CSILonghorn, l.Version))
		if l.Replicas != nil && *l.Replicas < 1 {
			addf("%s.longhorn.replicas must be at least 1", at)
		}
	}
	if selected[CSINFS] {
		if n := s.NFS; n == nil {
			addf("%s.nfs is required with csi nfs: the server and path of the export", at)
		} else {
			add(c.checkVersion(at+".nfs.version", CSINFS, n.Version))
			if !isHost(n.Server) {
				addf("%s.nfs.server %q must be the NFS server's address, like 192.168.1.5 or nas.lan", at, n.Server)
			}
			if !strings.HasPrefix(n.Path, "/") {
				addf("%s.nfs.path %q must be the exported directory, like /export/k8s", at, n.Path)
			}
		}
	}
	if selected[CSISMB] {
		if m := s.SMB; m == nil {
			addf("%s.smb is required with csi smb: source, usernameEnv and passwordEnv", at)
		} else {
			add(c.checkVersion(at+".smb.version", CSISMB, m.Version))
			if !smbSource.MatchString(m.Source) {
				addf("%s.smb.source %q must be a share like //nas/k8s", at, m.Source)
			}
			for _, f := range []struct{ name, v string }{{"usernameEnv", m.UsernameEnv}, {"passwordEnv", m.PasswordEnv}} {
				if !envName.MatchString(f.v) {
					addf("%s.smb.%s %q must name an environment variable, like SMB_USER (the credentials never go in the file)", at, f.name, f.v)
				}
			}
		}
	}
	return errs
}

// isHost accepts an IP address or a DNS name.
func isHost(v string) bool {
	if _, err := netip.ParseAddr(v); err == nil {
		return true
	}
	return len(v) <= 253 && dnsSubdomain.MatchString(strings.ToLower(v))
}
