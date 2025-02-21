package unidler

import (
	"net"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/proxy/userspace"
	"k8s.io/kubernetes/pkg/util/iptables"
	utilexec "k8s.io/utils/exec"

	unidlingapi "github.com/openshift/api/unidling/v1alpha1"
)

type NeedPodsSignaler interface {
	// NeedPods signals that endpoint addresses are needed in order to
	// service a traffic coming to the given service and port
	NeedPods(serviceName types.NamespacedName, port string) error
}

type eventSignaler struct {
	recorder events.EventRecorder
}

func (sig *eventSignaler) NeedPods(serviceName types.NamespacedName, port string) error {
	// TODO: we need to fake this since upstream removed our handle to the ObjectReference
	// This *should* be sufficient for the unidling controller
	serviceRef := v1.ObjectReference{
		Kind:      "Service",
		Namespace: serviceName.Namespace,
		Name:      serviceName.Name,
	}

	// HACK: make the message different to prevent event aggregation
	sig.recorder.Eventf(&serviceRef, nil, v1.EventTypeNormal, unidlingapi.NeedPodsReason, "The service-port %s:%s needs pods.", serviceRef.Name, port)

	return nil
}

// NewEventSignaler constructs a NeedPodsSignaler which signals by recording
// an event for the service with the "NeedPods" reason.
func NewEventSignaler(eventRecorder events.EventRecorder) NeedPodsSignaler {
	return &eventSignaler{
		recorder: eventRecorder,
	}
}

// NewUnidlerProxier creates a new Proxier for the given LoadBalancer and address which fires off
// unidling signals connections and traffic.  It is intended to be used as one half of a HybridProxier.
func NewUnidlerProxier(loadBalancer userspace.LoadBalancer, listenIP net.IP, iptables iptables.Interface, exec utilexec.Interface, pr utilnet.PortRange, syncPeriod, minSyncPeriod, udpIdleTimeout time.Duration, nodePortAddresses []string, signaler NeedPodsSignaler) (*userspace.Proxier, error) {
	newFunc := func(protocol v1.Protocol, ip net.IP, port int) (userspace.ProxySocket, error) {
		return newUnidlerSocket(protocol, ip, port, signaler)
	}
	return userspace.NewCustomProxier(loadBalancer, listenIP, iptables, exec, pr, syncPeriod, minSyncPeriod, udpIdleTimeout, nodePortAddresses, newFunc)
}
