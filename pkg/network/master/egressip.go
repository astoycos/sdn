package master

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	kcoreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/util/retry"

	osdnclient "github.com/openshift/client-go/network/clientset/versioned"
	osdninformers "github.com/openshift/client-go/network/informers/externalversions/network/v1"
	"github.com/openshift/sdn/pkg/network/common"
)

type egressIPManager struct {
	sync.Mutex

	tracker            *common.EgressIPTracker
	osdnClient         osdnclient.Interface
	hostSubnetInformer osdninformers.HostSubnetInformer
	nodeInformer       kcoreinformers.NodeInformer

	updatePending bool
	updatedAgain  bool

	monitorNodes map[string]*egressNode
	stop         chan struct{}
}

type egressNode struct {
	ip      string
	name    string
	offline bool
	retries int
}

func newEgressIPManager() *egressIPManager {
	eim := &egressIPManager{}
	eim.tracker = common.NewEgressIPTracker(eim)
	return eim
}

func (eim *egressIPManager) Start(osdnClient osdnclient.Interface, hostSubnetInformer osdninformers.HostSubnetInformer, netNamespaceInformer osdninformers.NetNamespaceInformer, nodeInformer kcoreinformers.NodeInformer) {
	eim.osdnClient = osdnClient
	eim.hostSubnetInformer = hostSubnetInformer
	eim.nodeInformer = nodeInformer
	eim.tracker.Start(hostSubnetInformer, netNamespaceInformer)
}

func (eim *egressIPManager) UpdateEgressCIDRs() {
	eim.Lock()
	defer eim.Unlock()

	// Coalesce multiple "UpdateEgressCIDRs" notifications into one by queueing
	// the update to happen a little bit later in a goroutine, and postponing that
	// update any time we get another "UpdateEgressCIDRs".

	if eim.updatePending {
		eim.updatedAgain = true
	} else {
		eim.updatePending = true
		go utilwait.PollInfinite(time.Second, eim.maybeDoUpdateEgressCIDRs)
	}
}

func (eim *egressIPManager) maybeDoUpdateEgressCIDRs() (bool, error) {
	eim.Lock()
	defer eim.Unlock()

	if eim.updatedAgain {
		eim.updatedAgain = false
		return false, nil
	}
	eim.updatePending = false

	// At this point it has been at least 1 second since the last "UpdateEgressCIDRs"
	// notification, so things are stable.
	//
	// ReallocateEgressIPs() will figure out what HostSubnets either can have new
	// egress IPs added to them, or need to have egress IPs removed from them, and
	// returns a map from node name to the new EgressIPs value, for each changed
	// HostSubnet.
	//
	// If a HostSubnet's EgressCIDRs changes while we are processing the reallocation,
	// we won't process that until this reallocation is complete.

	allocation := eim.tracker.ReallocateEgressIPs()
	monitorNodes := make(map[string]*egressNode, len(allocation))
	for nodeName, egressIPs := range allocation {
		resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			hs, err := eim.hostSubnetInformer.Lister().Get(nodeName)
			if err != nil {
				return err
			}

			if node := eim.monitorNodes[hs.HostIP]; node != nil {
				monitorNodes[hs.HostIP] = node
			} else {
				monitorNodes[hs.HostIP] = &egressNode{ip: hs.HostIP, name: nodeName}
			}

			oldIPs := sets.NewString(common.HSEgressIPsToStrings(hs.EgressIPs)...)
			newIPs := sets.NewString(egressIPs...)
			if !oldIPs.Equal(newIPs) {
				hs.EgressIPs = common.StringsToHSEgressIPs(egressIPs)
				_, err = eim.osdnClient.NetworkV1().HostSubnets().Update(context.TODO(), hs, metav1.UpdateOptions{})
			}
			return err
		})
		if resultErr != nil {
			utilruntime.HandleError(fmt.Errorf("Could not update HostSubnet EgressIPs: %v", resultErr))
		}
	}

	eim.monitorNodes = monitorNodes
	if len(monitorNodes) > 0 {
		if eim.stop == nil {
			eim.stop = make(chan struct{})
			go eim.poll(eim.stop)
		}
	} else {
		if eim.stop != nil {
			close(eim.stop)
			eim.stop = nil
		}
	}

	return true, nil
}

const (
	pollInterval   = 5 * time.Second
	repollInterval = time.Second
	maxRetries     = 2
)

func (eim *egressIPManager) poll(stop chan struct{}) {
	retry := false
	for {
		select {
		case <-stop:
			return
		default:
		}

		start := time.Now()
		retry, err := eim.check(retry)
		if err != nil {
			klog.Warningf("Node may have been deleted or not exist anymore")
		}
		if !retry {
			// If less than pollInterval has passed since start, then sleep until it has
			time.Sleep(start.Add(pollInterval).Sub(time.Now()))
		}
	}
}

func nodeIsReady(node *corev1.Node) bool {
	nodeReady := true
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionFalse || cond.Status == corev1.ConditionUnknown {
				nodeReady = false
			}
		}
	}
	return nodeReady
}

func (eim *egressIPManager) check(retrying bool) (bool, error) {
	var timeout time.Duration
	if retrying {
		timeout = repollInterval
	} else {
		timeout = pollInterval
	}

	needRetry := false
	for _, node := range eim.monitorNodes {
		if retrying && node.retries == 0 {
			continue
		}

		nn, err := eim.nodeInformer.Lister().Get(node.name)
		if err != nil {
			return false, err
		}

		if !nodeIsReady(nn) {
			klog.Warningf("Node %s is not Ready", node.name)
			node.offline = true
			eim.tracker.SetNodeOffline(node.ip, true)
			// Return when there's a not Ready node
			return false, nil
		}

		online := eim.tracker.Ping(node.ip, timeout)
		if node.offline && online {
			klog.Infof("Node %s is back online", node.ip)
			node.offline = false
			eim.tracker.SetNodeOffline(node.ip, false)
		} else if !node.offline && !online {
			node.retries++
			if node.retries > maxRetries {
				klog.Warningf("Node %s is offline", node.ip)
				node.retries = 0
				node.offline = true
				eim.tracker.SetNodeOffline(node.ip, true)
			} else {
				klog.V(2).Infof("Node %s may be offline... retrying", node.ip)
				needRetry = true
			}
		}
	}

	return needRetry, nil
}

func (eim *egressIPManager) Synced() {
}

func (eim *egressIPManager) ClaimEgressIP(vnid uint32, egressIP, nodeIP string) {
}

func (eim *egressIPManager) ReleaseEgressIP(egressIP, nodeIP string) {
}

func (eim *egressIPManager) SetNamespaceEgressNormal(vnid uint32) {
}

func (eim *egressIPManager) SetNamespaceEgressDropped(vnid uint32) {
}

func (eim *egressIPManager) SetNamespaceEgressViaEgressIPs(vnid uint32, activeEgressIPs []common.EgressIPAssignment) {
}
