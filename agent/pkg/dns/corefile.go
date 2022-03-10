package dns

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"net"
	"strconv"
	"strings"

	// blank imports to make sure the plugin code is pulled in from vendor
	_ "github.com/coredns/coredns/plugin/bind"
	_ "github.com/coredns/coredns/plugin/cache"
	_ "github.com/coredns/coredns/plugin/debug"
	_ "github.com/coredns/coredns/plugin/dns64"
	_ "github.com/coredns/coredns/plugin/errors"
	_ "github.com/coredns/coredns/plugin/forward"
	_ "github.com/coredns/coredns/plugin/health"
	_ "github.com/coredns/coredns/plugin/hosts"
	_ "github.com/coredns/coredns/plugin/kubernetes"
	_ "github.com/coredns/coredns/plugin/loadbalance"
	_ "github.com/coredns/coredns/plugin/log"
	_ "github.com/coredns/coredns/plugin/loop"
	_ "github.com/coredns/coredns/plugin/metrics"
	_ "github.com/coredns/coredns/plugin/pprof"
	_ "github.com/coredns/coredns/plugin/reload"
	_ "github.com/coredns/coredns/plugin/rewrite"
	_ "github.com/coredns/coredns/plugin/template"
	_ "github.com/coredns/coredns/plugin/trace"
	_ "github.com/coredns/coredns/plugin/whoami"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	appconfig "github.com/kubeedge/edgemesh/agent/cmd/edgemesh-agent/app/config"
	"github.com/kubeedge/edgemesh/agent/pkg/dns/config"
	"github.com/kubeedge/edgemesh/common/informers"
	"github.com/kubeedge/edgemesh/common/util"
)

// copy from https://github.com/kubernetes/dns/blob/1.21.0/cmd/node-cache/app/configmap.go and update
const (
	stubDomainBlock = `{{.DomainName}}:{{.Port}} {
    bind {{.LocalIP}}
    cache {{.CacheTTL}}
    errors
    forward . {{.UpstreamServers}} {
        force_tcp
    }
    {{ .KubernetesPlugin }}
    log
    loop
    reload
}
`
	kubernetesPluginBlock = `kubernetes cluster.local in-addr.arpa ip6.arpa {
        {{ .APIServer }}
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
        ttl {{ .TTL }}
    }`
	defaultTTL            = 30
	defaultUpstreamServer = "/etc/resolv.conf"
	corefilePath          = "Corefile"
)

// copy from https://github.com/kubernetes/dns/blob/1.21.0/cmd/node-cache/app/configmap.go and update
// stubDomainInfo contains all the parameters needed to compute
// a stubDomain block in the Corefile.
type stubDomainInfo struct {
	DomainName       string
	LocalIP          string
	Port             string
	CacheTTL         int
	UpstreamServers  string
	KubernetesPlugin string
}

type KubernetesPluginInfo struct {
	APIServer string
	TTL       int
}

func getKubernetesPluginStr(cfg *config.EdgeDNSConfig) (string, error) {
	var apiServer string
	if cfg.Mode == appconfig.DebugMode {
		if cfg.KubeAPIConfig.Master != "" {
			apiServer = fmt.Sprintf("endpoint %s", cfg.KubeAPIConfig.Master)
		}
		// if kubeconfig is set, use it to overwrite the endpoint
		if cfg.KubeAPIConfig.KubeConfig != "" {
			apiServer = fmt.Sprintf("kubeconfig %s", cfg.KubeAPIConfig.KubeConfig)
		}
	} else if cfg.Mode == appconfig.EdgeMode {
		apiServer = fmt.Sprintf("endpoint %s", appconfig.DefaultEdgeApiServer)
	}

	info := &KubernetesPluginInfo{
		APIServer: apiServer,
		TTL:       defaultTTL,
	}
	var tpl bytes.Buffer
	tmpl, err := template.New("kubernetesPluginBlock").Parse(kubernetesPluginBlock)
	if err != nil {
		return "", fmt.Errorf("failed to create kubernetesPlugin template, err : %w", err)
	}
	if err := tmpl.Execute(&tpl, *info); err != nil {
		return "", fmt.Errorf("failed to create kubernetesPlugin template, err : %w", err)
	}
	return tpl.String(), nil
}

// copy from https://github.com/kubernetes/dns/blob/1.21.0/cmd/node-cache/app/configmap.go and update
func getStubDomainStr(stubDomainMap map[string][]string, info *stubDomainInfo) (string, error) {
	var tpl bytes.Buffer
	for domainName, servers := range stubDomainMap {
		tmpl, err := template.New("stubDomainBlock").Parse(stubDomainBlock)
		if err != nil {
			return "", fmt.Errorf("failed to create stubDomain template, err : %w", err)
		}
		info.DomainName = domainName
		info.UpstreamServers = strings.Join(servers, " ")
		if err := tmpl.Execute(&tpl, *info); err != nil {
			return "", fmt.Errorf("failed to create stubDomain template, err : %w", err)
		}
	}
	return tpl.String(), nil
}

// copy from https://github.com/kubernetes/dns/blob/1.21.0/cmd/node-cache/app/configmap.go and update
func UpdateCorefile(cfg *config.EdgeDNSConfig, ifm *informers.Manager) error {
	// get listen ip
	ListenIP, err := util.GetInterfaceIP(cfg.ListenInterface)
	if err != nil {
		return err
	}

	cacheTTL := defaultTTL
	upstreamServers := []string{defaultUpstreamServer}
	kubernetesPlugin, err := getKubernetesPluginStr(cfg)
	if err != nil {
		return err
	}

	if cfg.CacheDNS.Enable {
		// Reset upstream server
		upstreamServers = []string{}
		if cfg.CacheDNS.AutoDetect {
			upstreamServers = append(upstreamServers, detectClusterDNS(ifm.GetKubeClient())...)
		}
		for _, server := range cfg.CacheDNS.UpstreamServers {
			server = strings.TrimSpace(server)
			if server == "" {
				continue
			}
			if isValidAddress(server) {
				upstreamServers = append(upstreamServers, server)
			} else {
				klog.Errorf("Invalid address: %s", server)
			}
		}
		upstreamServers = removeDuplicate(upstreamServers)
		if len(upstreamServers) == 0 {
			return fmt.Errorf("failed to get nodelocal dns upstream servers")
		} else {
			klog.Infof("nodelocal dns upstream servers: %v", upstreamServers)
		}
		cacheTTL = cfg.CacheDNS.CacheTTL
		// Disable coredns kubernetes plugin.
		kubernetesPlugin = ""
	}

	stubDomainMap := make(map[string][]string)
	stubDomainMap["."] = upstreamServers
	stubDomainStr, err := getStubDomainStr(stubDomainMap, &stubDomainInfo{
		LocalIP:          ListenIP.String(),
		Port:             fmt.Sprintf("%d", cfg.ListenPort),
		CacheTTL:         cacheTTL,
		KubernetesPlugin: kubernetesPlugin,
	})
	if err != nil {
		return err
	}

	newConfig := bytes.Buffer{}
	newConfig.WriteString(stubDomainStr)
	if err := ioutil.WriteFile(corefilePath, newConfig.Bytes(), 0666); err != nil {
		return fmt.Errorf("failed to write %s, err %w", corefilePath, err)
	}

	return nil
}

func detectClusterDNS(kubeClient kubernetes.Interface) (servers []string) {
	coredns, err := kubeClient.CoreV1().Services("kube-system").Get(context.Background(), "coredns", metav1.GetOptions{})
	if err == nil && coredns.Spec.ClusterIP != v1.ClusterIPNone {
		servers = append(servers, coredns.Spec.ClusterIP)
	}
	kubedns, err := kubeClient.CoreV1().Services("kube-system").Get(context.Background(), "kube-dns", metav1.GetOptions{})
	if err == nil && kubedns.Spec.ClusterIP != v1.ClusterIPNone {
		servers = append(servers, kubedns.Spec.ClusterIP)
	}
	kubeDNSList, err := kubeClient.CoreV1().Services("kube-system").List(context.Background(), metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
	if err == nil {
		for _, item := range kubeDNSList.Items {
			if item.Spec.ClusterIP != v1.ClusterIPNone {
				servers = append(servers, item.Spec.ClusterIP)
			}
		}
	}
	servers = removeDuplicate(servers)
	if len(servers) == 0 {
		klog.Errorf("Unable to automatically detect cluster dns. Do you have coredns or kube-dns installed in your cluster?")
	} else {
		klog.Infof("Automatically detect cluster dns: %v", servers)
	}
	return servers
}

func isValidAddress(addr string) bool {
	items := strings.Split(addr, ":")
	if len(items) == 1 {
		return isValidIP(items[0])
	} else if len(items) == 2 {
		return isValidIP(items[0]) && isValidPort(items[1])
	}
	return false
}

func isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

func isValidPort(port string) bool {
	pnum, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	if 0 < pnum && pnum < 65536 {
		return true
	}
	return false
}

func removeDuplicate(ss []string) []string {
	ret := make([]string, 0)
	tmp := make(map[string]struct{})
	for _, s := range ss {
		if _, ok := tmp[s]; !ok {
			ret = append(ret, s)
			tmp[s] = struct{}{}
		}
	}
	return ret
}
