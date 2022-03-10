package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/kubeedge/edgemesh/agent/pkg/proxy/protocol"
	"github.com/kubeedge/edgemesh/agent/pkg/tunnel"
	"github.com/kubeedge/edgemesh/agent/pkg/tunnel/proxy"
	"github.com/kubeedge/edgemesh/common/constants"
	"github.com/kubeedge/edgemesh/common/util"
)

const (
	LabelKubeedge string = "kubeedge=edgemesh-agent"
	AgentPodName  string = "edgemesh-agent"

	// Version is socks5 version
	Version byte = 0x05

	// DefaultMethod is No certification required
	DefaultMethod byte = 0x00

	Success byte = 0x00

	// ATYPIPv4 is ipv4 address type
	ATYPIPv4 byte = 0x01 // 4 octets
	// ATYPDomain is domain address type
	ATYPDomain byte = 0x03 // The first octet of the address field contains the number of octets of name that follow, there is no terminating NUL octet.
	// ATYPIPv6 is ipv6 address type
	ATYPIPv6 byte = 0x04 // 16 octets

	// CmdConnect is connect command
	CmdConnect byte = 0x01
)

// DefaultResponse is Socks5 returns data by default
var DefaultResponse = []byte{Version, Success, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// copy form https://github.com/txthinking/socks5/blob/e03c1217a50bd1363a2aaf58290da622256704fa/socks5.go#from L86 and update
type Request struct {
	Version     byte
	Command     byte
	Rsv         byte
	AddressType byte
	DstAddr     string
	DstPort     int32
}

type SocksHandle struct {
	Request *Request
}

type Socks5Proxy struct {
	TCPProxy    *protocol.TCPProxy
	kubeClient  kubernetes.Interface
	NodeName    string
	SocksHandle *SocksHandle
}

func (s *SocksHandle) ParsingConnect(conn net.Conn) (err error) {
	err = s.handShake(conn)
	if err != nil {
		return err
	}

	err = s.NewRequest(conn)
	if err != nil {
		return err
	}

	return nil
}

// copy from https://github.com/txthinking/socks5/blob/e03c1217a50bd1363a2aaf58290da622256704fa/server_side.go#L18 and update
func (s *SocksHandle) handShake(conn net.Conn) (err error) {
	data := make([]byte, 2)
	_, err = conn.Read(data)
	if err != nil {
		return err
	}

	if data[0] != Version {
		return fmt.Errorf("invalid version")
	}

	if data[1] == 0 {
		return fmt.Errorf("method length error")
	}

	ms := make([]byte, int(data[1]))
	_, err = conn.Read(ms)
	if err != nil {
		return err
	}

	flag := false
	var m byte
	for _, m = range ms {
		if m == DefaultMethod {
			flag = true
		}
	}
	if !flag {
		return fmt.Errorf("this method is not yet supported")
	}

	_, err = conn.Write([]byte{Version, Success})
	if err != nil {
		return err
	}

	return nil
}

// copy from https://github.com/txthinking/socks5/blob/e03c1217a50bd1363a2aaf58290da622256704fa/server_side.go#L125 and update
func (s *SocksHandle) NewRequest(conn net.Conn) (err error) {
	data := make([]byte, 4)
	_, err = conn.Read(data)
	if err != nil {
		return err
	}

	if data[0] != Version {
		return fmt.Errorf("invalid version")
	}

	var addr []byte
	var host string
	if data[3] == ATYPIPv4 {
		addr = make([]byte, 4)
		if _, err := conn.Read(addr); err != nil {
			return err
		}
		host = net.IP(addr).String()
	} else if data[3] == ATYPIPv6 {
		addr = make([]byte, 16)
		if _, err := conn.Read(addr); err != nil {
			return err
		}
		host = net.IP(addr).String()
	} else if data[3] == ATYPDomain {
		dal := make([]byte, 1)
		if _, err := conn.Read(dal); err != nil {
			return err
		}

		addr = make([]byte, int(dal[0]))
		if _, err := conn.Read(addr); err != nil {
			return err
		}
		host = string(addr)
	} else {
		return fmt.Errorf("destination address is incorrect")
	}

	port := make([]byte, 2)
	if _, err := conn.Read(port); err != nil {
		return err
	}

	s.Request = &Request{
		data[0],
		data[1],
		data[2],
		data[3],
		host,
		int32(binary.BigEndian.Uint16(port)),
	}
	return nil
}

func (s *Socks5Proxy) Start() {
	go func() {
		for {
			conn, err := s.TCPProxy.Listener.Accept()
			if err != nil {
				klog.Warningf("get socks5 tcp conn error: %v", err)
				continue
			}
			go s.HandleSocksProxy(conn)
		}
	}()
}

func NewSocks5Proxy(ip net.IP, port int, NodeName string, kubeClient kubernetes.Interface) (socks5Proxy *Socks5Proxy, err error) {
	socks := &Socks5Proxy{
		kubeClient: kubeClient,
		TCPProxy:   &protocol.TCPProxy{Name: protocol.TCP},
		NodeName:   NodeName,
		SocksHandle: &SocksHandle{
			Request: &Request{},
		},
	}

	if err := socks.TCPProxy.SetListener(ip, port); err != nil {
		return socks, fmt.Errorf("set socks5 proxy err: %v, host: %s, port: %d", err, ip, port)
	}
	return socks, nil
}

func (s *Socks5Proxy) HandleSocksProxy(conn net.Conn) {
	if conn == nil {
		return
	}
	defer conn.Close()

	err := s.SocksHandle.ParsingConnect(conn)

	if err != nil {
		klog.Errorf("Request parsing error. %v", err)
		return
	}

	if s.SocksHandle.Request.AddressType != ATYPDomain || s.SocksHandle.Request.DstAddr == s.NodeName {
		klog.Warningf("Connecting to the local computer and connecting via IP are not supported. host: %s, port: %d, localNodeName: %s", s.SocksHandle.Request.DstAddr, s.SocksHandle.Request.DstPort, s.NodeName)
		return
	}

	targetIP, err := s.getTargetIpByNodeName(s.SocksHandle.Request.DstAddr)
	if err != nil {
		klog.Errorf("Unable to get destination IP, %v", err)
		return
	}
	klog.Info("Successfully get destination IP. NodeIP: ", targetIP, ", Port: ", s.SocksHandle.Request.DstPort)

	if s.SocksHandle.Request.Command == CmdConnect {
		proxyConnectToRemote(s.SocksHandle.Request.DstAddr, targetIP, s.SocksHandle.Request.DstPort, conn)
	} else {
		klog.Warningf("this method is not yet supported. command: %v", s.SocksHandle.Request.Command)
	}
}

func proxyConnectToRemote(host string, targetIP string, port int32, conn net.Conn) {
	proxyOpts := proxy.ProxyOptions{
		Protocol: "tcp",
		NodeName: host,
		IP:       targetIP,
		Port:     port,
	}
	stream, err := tunnel.Agent.ProxySvc.GetProxyStream(proxyOpts)
	if err != nil {
		klog.Errorf("l4 proxy get proxy stream from %s error: %w", host, err)
		return
	}

	klog.Infof("l4 proxy start proxy data between tcpserver %v", host)
	_, err = conn.Write(DefaultResponse)
	if err != nil {
		klog.Errorf("return corresponding data error: %v", err)
	}

	go util.ProxyConn(stream, conn)

	klog.Infof("Success proxy to %v", host)
}

// getTargetIpByNodeName Returns the real IP address of the node
// We must obtain the real IP address of the node to communicate, so we need to query the IP address of the edgemesh-agent on the node
// Because users may modify the IP addresses of edgemesh-0 and edgecore. If used directly, it may cause errors
func (s *Socks5Proxy) getTargetIpByNodeName(nodeName string) (targetIP string, err error) {
	pods, err := s.kubeClient.CoreV1().Pods(constants.EdgeMeshNamespace).List(context.Background(), metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName, LabelSelector: LabelKubeedge})
	if err != nil {
		return "", err
	}
	ip, err := "", fmt.Errorf("edgemesh agent not found on node [%s]", nodeName)
	for _, pod := range pods.Items {
		if strings.Contains(pod.Name, AgentPodName) {
			ip = pod.Status.PodIP
			err = nil
		}
	}

	return ip, err
}
