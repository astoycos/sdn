package node

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"

	osdninformers "github.com/openshift/client-go/network/informers/externalversions"
	"github.com/openshift/sdn/pkg/network/common"
	"github.com/vishvananda/netlink"
)

const (
	defaultPollInterval = 5 * time.Second
	repollInterval      = time.Second
	maxRetries          = 2
)

type egressNode struct {
	nodeIP  string
	offline bool

	egressIPs sets.String
	retries   int
}

type egressIPWatcher struct {
	// We don't need a mutex because tracker serializes all of its callbacks to us

	tracker *common.EgressIPTracker

	oc            *ovsController
	localIP       string
	masqueradeBit uint32

	iptables     *NodeIPTables
	iptablesMark map[string]string

	monitorNodesLock sync.Mutex
	monitorNodes     map[string]*egressNode
	stop             chan struct{}

	testModeChan chan string
}

type egressIPMetaData struct {
	nodeIP     string
	packetMark string
}

func newEgressIPWatcher(oc *ovsController, localIP string, masqueradeBit *int32) *egressIPWatcher {
	eip := &egressIPWatcher{
		oc:           oc,
		localIP:      localIP,
		monitorNodes: make(map[string]*egressNode),
		iptablesMark: make(map[string]string),
	}
	if masqueradeBit != nil {
		eip.masqueradeBit = 1 << uint32(*masqueradeBit)
	}

	eip.tracker = common.NewEgressIPTracker(eip)
	return eip
}

func (eip *egressIPWatcher) Start(osdnInformers osdninformers.SharedInformerFactory, iptables *NodeIPTables) error {
	eip.iptables = iptables
	eip.tracker.Start(osdnInformers.Network().V1().HostSubnets(), osdnInformers.Network().V1().NetNamespaces())
	return nil
}

func (eip *egressIPWatcher) Synced() {
	link, _, err := GetLinkDetails(eip.localIP)
	if err != nil {
		// shouldn't happen, but obviously there's nothing to clean up...
		return
	}
	label, err := egressIPLabel(link)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Could not check for stale egress IPs: %v", err))
		return
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Could not check for stale egress IPs: %v", err))
		return
	}

	for _, addr := range addrs {
		ip := addr.IP.String()
		if addr.Label == label && eip.iptablesMark[ip] == "" {
			klog.Infof("Cleaning up stale egress IP %s", addr.IP.String())
			err = netlink.AddrDel(link, &addr)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("Could not clean up stale egress IP: %v", err))
			}
		}
	}

	eip.iptables.SyncEgressIPRules()
}

func egressIPLabel(link netlink.Link) (string, error) {
	// An address label must start with the link name plus ":", and must be at most 15
	// characters long. If the link name is too long then we can't label egress IPs.
	label := link.Attrs().Name + ":eip"
	if len(label) > 15 {
		return "", fmt.Errorf("link name %q is too long", link.Attrs().Name)
	}
	return label, nil
}

// Convert vnid to a hex value that is not 0, does not have masqueradeBit set, and isn't
// the same value as would be returned for any other valid vnid.
func getMarkForVNID(vnid, masqueradeBit uint32) string {
	if vnid == 0 {
		vnid = 0xff000000
	}
	if (vnid & masqueradeBit) != 0 {
		vnid = (vnid | 0x01000000) ^ masqueradeBit
	}
	return fmt.Sprintf("0x%08x", vnid)
}

func (eip *egressIPWatcher) ClaimEgressIP(vnid uint32, egressIP, nodeIP string) {
	if nodeIP == eip.localIP {
		mark := getMarkForVNID(vnid, eip.masqueradeBit)
		eip.iptablesMark[egressIP] = mark
		if err := eip.assignEgressIP(egressIP, mark); err != nil {
			utilruntime.HandleError(fmt.Errorf("Error assigning Egress IP %q: %v", egressIP, err))
		}
	} else {
		eip.addEgressIP(nodeIP, egressIP)
	}
}

func (eip *egressIPWatcher) ReleaseEgressIP(egressIP, nodeIP string) {
	if nodeIP == eip.localIP {
		mark := eip.iptablesMark[egressIP]
		delete(eip.iptablesMark, egressIP)
		if err := eip.releaseEgressIP(egressIP, mark); err != nil {
			utilruntime.HandleError(fmt.Errorf("Error releasing Egress IP %q: %v", egressIP, err))
		}
	} else {
		eip.removeEgressIP(nodeIP, egressIP)
	}
}

func (eip *egressIPWatcher) addEgressIP(nodeIP, egressIP string) {
	eip.monitorNodesLock.Lock()
	defer eip.monitorNodesLock.Unlock()

	if eip.monitorNodes[nodeIP] != nil {
		eip.monitorNodes[nodeIP].egressIPs.Insert(egressIP)
		return
	}
	klog.V(4).Infof("Monitoring node %s", nodeIP)

	eip.monitorNodes[nodeIP] = &egressNode{
		nodeIP:    nodeIP,
		egressIPs: sets.NewString(egressIP),
	}
	if len(eip.monitorNodes) == 1 {
		eip.stop = make(chan struct{})
		go utilwait.PollUntil(defaultPollInterval, eip.poll, eip.stop)
	}
}

func (eip *egressIPWatcher) removeEgressIP(nodeIP, egressIP string) {
	eip.monitorNodesLock.Lock()
	defer eip.monitorNodesLock.Unlock()

	if eip.monitorNodes[nodeIP] == nil {
		return
	}
	eip.monitorNodes[nodeIP].egressIPs.Delete(egressIP)
	if eip.monitorNodes[nodeIP].egressIPs.Len() == 0 {
		klog.V(4).Infof("Unmonitoring node %s", nodeIP)
		delete(eip.monitorNodes, nodeIP)
		if len(eip.monitorNodes) == 0 && eip.stop != nil {
			close(eip.stop)
			eip.stop = nil
		}
	}
}

func (eip *egressIPWatcher) poll() (bool, error) {
	retry := eip.check(false)
	for retry {
		time.Sleep(repollInterval)
		retry = eip.check(true)
	}
	return false, nil
}

func (eip *egressIPWatcher) check(retrying bool) bool {
	offlineResult, needRetry := eip.getOfflineResult(retrying)
	for nodeIP, offline := range offlineResult {
		eip.tracker.SetNodeOffline(nodeIP, offline)
	}
	return needRetry
}

func (eip *egressIPWatcher) getOfflineResult(retrying bool) (map[string]bool, bool) {
	eip.monitorNodesLock.Lock()
	defer eip.monitorNodesLock.Unlock()

	var timeout time.Duration
	if retrying {
		timeout = repollInterval
	} else {
		timeout = defaultPollInterval
	}

	needRetry := false
	offlineResult := make(map[string]bool)
	for _, node := range eip.monitorNodes {
		if retrying && node.retries == 0 {
			continue
		}

		online := eip.tracker.Ping(node.nodeIP, timeout)
		if node.offline && online {
			klog.Infof("Node %s is back online", node.nodeIP)
			node.offline = false
			offlineResult[node.nodeIP] = false
		} else if !node.offline && !online {
			node.retries++
			if node.retries > maxRetries {
				klog.Warningf("Node %s is offline", node.nodeIP)
				node.retries = 0
				node.offline = true
				offlineResult[node.nodeIP] = true
			} else {
				klog.V(2).Infof("Node %s may be offline... retrying", node.nodeIP)
				needRetry = true
			}
		}
	}
	return offlineResult, needRetry
}

func (eip *egressIPWatcher) UpdateEgressCIDRs() {
}

func (eip *egressIPWatcher) SetNamespaceEgressNormal(vnid uint32) {
	if err := eip.oc.SetNamespaceEgressNormal(vnid); err != nil {
		utilruntime.HandleError(fmt.Errorf("Error updating Namespace egress rules for VNID %d: %v", vnid, err))
	}
}

func (eip *egressIPWatcher) SetNamespaceEgressDropped(vnid uint32) {
	if err := eip.oc.SetNamespaceEgressDropped(vnid); err != nil {
		utilruntime.HandleError(fmt.Errorf("Error updating Namespace egress rules for VNID %d: %v", vnid, err))
	}
}

func (eip *egressIPWatcher) SetNamespaceEgressViaEgressIPs(vnid uint32, activeEgressIPs []common.EgressIPAssignment) {
	egressIPsMetaData := []egressIPMetaData{}
	for _, egressIPAssignment := range activeEgressIPs {
		egressIPsMetaData = append(egressIPsMetaData, egressIPMetaData{nodeIP: egressIPAssignment.NodeIP, packetMark: eip.iptablesMark[egressIPAssignment.EgressIP]})
	}
	if err := eip.oc.SetNamespaceEgressViaEgressIPs(vnid, egressIPsMetaData); err != nil {
		utilruntime.HandleError(fmt.Errorf("Error updating Namespace egress rules for VNID %d: %v", vnid, err))
	}
}

func (eip *egressIPWatcher) assignEgressIP(egressIP, mark string) error {
	if egressIP == eip.localIP {
		return fmt.Errorf("desired egress IP %q is the node IP", egressIP)
	}

	if eip.testModeChan != nil {
		eip.testModeChan <- fmt.Sprintf("claim %s", egressIP)
		return nil
	}

	localEgressLink, localEgressNet, err := GetLinkDetails(eip.localIP)
	if err != nil {
		return fmt.Errorf("unable to get egress link details: %v", err)
	}

	localEgressIPMaskLen, _ := localEgressNet.Mask.Size()
	egressIPNet := fmt.Sprintf("%s/%d", egressIP, localEgressIPMaskLen)
	addr, err := netlink.ParseAddr(egressIPNet)
	if err != nil {
		return fmt.Errorf("could not parse egress IP %q: %v", egressIPNet, err)
	}
	if !localEgressNet.Contains(addr.IP) {
		return fmt.Errorf("egress IP %q is not in local network %s of interface %s", egressIP, localEgressNet.String(), localEgressLink.Attrs().Name)
	}
	addr.Label, _ = egressIPLabel(localEgressLink)
	err = netlink.AddrAdd(localEgressLink, addr)
	if err != nil {
		if err == syscall.EEXIST {
			klog.V(2).Infof("Egress IP %q already exists on %s", egressIPNet, localEgressLink.Attrs().Name)
		} else {
			return fmt.Errorf("could not add egress IP %q to %s: %v", egressIPNet, localEgressLink.Attrs().Name, err)
		}
	}
	// Use arping to try to update other hosts ARP caches, in case this IP was
	// previously active on another node. (Based on code from "ifup".)
	go func() {
		out, err := exec.Command("/sbin/arping", "-q", "-A", "-c", "1", "-I", localEgressLink.Attrs().Name, egressIP).CombinedOutput()
		if err != nil {
			klog.Warningf("Failed to send ARP claim for egress IP %q: %v (%s)", egressIP, err, string(out))
			return
		}
		time.Sleep(2 * time.Second)
		_ = exec.Command("/sbin/arping", "-q", "-U", "-c", "1", "-I", localEgressLink.Attrs().Name, egressIP).Run()
	}()

	if err := eip.iptables.AddEgressIPRules(egressIP, mark); err != nil {
		return fmt.Errorf("could not add egress IP iptables rule: %v", err)
	}

	return nil
}

func (eip *egressIPWatcher) releaseEgressIP(egressIP, mark string) error {
	if egressIP == eip.localIP {
		return nil
	}

	if eip.testModeChan != nil {
		eip.testModeChan <- fmt.Sprintf("release %s", egressIP)
		return nil
	}

	localEgressLink, localEgressNet, err := GetLinkDetails(eip.localIP)
	if err != nil {
		return fmt.Errorf("unable to get egress link details: %v", err)
	}

	localEgressIPMaskLen, _ := localEgressNet.Mask.Size()
	egressIPNet := fmt.Sprintf("%s/%d", egressIP, localEgressIPMaskLen)
	addr, err := netlink.ParseAddr(egressIPNet)
	if err != nil {
		return fmt.Errorf("could not parse egress IP %q: %v", egressIPNet, err)
	}
	err = netlink.AddrDel(localEgressLink, addr)
	if err != nil {
		if err == syscall.EADDRNOTAVAIL {
			klog.V(2).Infof("Could not delete egress IP %q from %s: no such address", egressIPNet, localEgressLink.Attrs().Name)
		} else {
			return fmt.Errorf("could not delete egress IP %q from %s: %v", egressIPNet, localEgressLink.Attrs().Name, err)
		}
	}

	if err := eip.iptables.DeleteEgressIPRules(egressIP, mark); err != nil {
		return fmt.Errorf("could not delete egress IP iptables rule: %v", err)
	}

	return nil
}
