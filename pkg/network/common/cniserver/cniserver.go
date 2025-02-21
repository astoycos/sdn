package cniserver

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"k8s.io/klog/v2"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
)

// *** The CNIServer is PRIVATE API between OpenShift SDN components and may be
// changed at any time.  It is in no way a supported interface or API. ***
//
// The CNIServer accepts pod setup/teardown requests from the OpenShift SDN
// CNI plugin, which is itself called by openshift-node when pod networking
// should be set up or torn down.  The OpenShift SDN CNI plugin gathers up
// the standard CNI environment variables and network configuration provided
// on stdin and forwards them to the CNIServer over a private, root-only
// Unix domain socket, using HTTP as the transport and JSON as the protocol.
//
// The CNIServer interprets standard CNI environment variables as specified
// by the Container Network Interface (CNI) specification available here:
// https://github.com/containernetworking/cni/blob/master/SPEC.md
// While the CNIServer interface is not itself versioned, as the CNI
// specification requires that CNI network configuration is versioned, and
// since the OpenShift SDN CNI plugin passes that configuration to the
// CNIServer, versioning is ensured in exactly the same way as an executable
// CNI plugin would be versioned.
//
// Security: since the Unix domain socket created by the CNIServer is owned
// by root and inaccessible to any other user, no unprivileged process may
// access the CNIServer.  The Unix domain socket and its parent directory are
// removed and re-created with 0700 permissions each time openshift-node is
// started.

// Default directory for CNIServer runtime files
const CNIServerRunDir string = "/var/run/openshift-sdn/cniserver"

// CNIServer socket name, and default full path
const CNIServerSocketName string = "socket"
const CNIServerSocketPath string = CNIServerRunDir + "/" + CNIServerSocketName

// Config file contains server to plugin config data
const CNIServerConfigFileName string = "config.json"
const CNIServerConfigFilePath string = CNIServerRunDir + "/" + CNIServerConfigFileName

// Server-to-plugin config data
type Config struct {
	MTU                uint32 `json:"mtu"`
	ServiceNetworkCIDR string `json:"serviceNetworkCIDR"`
}

// Explicit type for CNI commands the server handles
type CNICommand string

const CNI_ADD CNICommand = "ADD"
const CNI_UPDATE CNICommand = "UPDATE"
const CNI_DEL CNICommand = "DEL"

// Request sent to the CNIServer by the OpenShift SDN CNI plugin
type CNIRequest struct {
	// CNI environment variables, like CNI_COMMAND and CNI_NETNS
	Env map[string]string `json:"env,omitempty"`
	// CNI configuration passed via stdin to the CNI plugin
	Config []byte `json:"config,omitempty"`
	// Host side of the veth pair (for an ADD command)
	HostVeth string `json:"hostVeth,omitempty"`
}

// Request structure built from CNIRequest which is passed to the
// handler function given to the CNIServer at creation time
type PodRequest struct {
	// The CNI command of the operation
	Command CNICommand
	// kubernetes namespace name
	PodNamespace string
	// kubernetes pod name
	PodName string
	// kubernetes container ID
	SandboxID string
	// kernel network namespace path
	Netns string
	// for an ADD request, the host side of the created veth
	HostVeth string
	// for an ADD request, the (optional) already-assigned IP
	AssignedIP string
	// Channel for returning the operation result to the CNIServer
	Result chan *PodResult
}

// Result of a PodRequest sent through the PodRequest's Result channel.
type PodResult struct {
	// Response to be returned to the OpenShift SDN CNI plugin on success
	Response []byte
	// Error to be returned to the OpenShift SDN CNI plugin on failure
	Err error
}

type cniRequestFunc func(request *PodRequest) ([]byte, error)

// CNI server object that listens for JSON-marshaled CNIRequest objects
// on a private root-only Unix domain socket.
type CNIServer struct {
	http.Server
	requestFunc cniRequestFunc
	rundir      string
	config      *Config
}

// Create and return a new CNIServer object which will listen on a socket in the given path
func NewCNIServer(rundir string, config *Config) *CNIServer {
	router := mux.NewRouter()

	s := &CNIServer{
		Server: http.Server{
			Handler: router,
		},
		rundir: rundir,
		config: config,
	}
	router.NotFoundHandler = http.HandlerFunc(http.NotFound)
	router.HandleFunc("/", s.handleCNIRequest).Methods("POST")
	return s
}

// Start the CNIServer's local HTTP server on a root-owned Unix domain socket.
// requestFunc will be called to handle pod setup/teardown operations on each
// request to the CNIServer's HTTP server, and should return a PodResult
// when the operation has completed.
func (s *CNIServer) Start(requestFunc cniRequestFunc) error {
	if requestFunc == nil {
		return fmt.Errorf("no pod request handler")
	}
	s.requestFunc = requestFunc

	configPath := filepath.Join(s.rundir, CNIServerConfigFileName)
	socketPath := filepath.Join(s.rundir, CNIServerSocketName)

	// If our socket directory exists, make sure it is private and empty
	info, err := os.Stat(s.rundir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("could not read CNIServer directory: %v", err)
		}
	} else if info.IsDir() && info.Mode().Perm() == 0700 {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove old CNIServer socket: %v", err)
		}
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove old CNIServer config: %v", err)
		}
	} else {
		if err := os.RemoveAll(s.rundir); err != nil {
			return fmt.Errorf("failed to remove old CNIServer directory: %v", err)
		}
	}

	if err := os.MkdirAll(s.rundir, 0700); err != nil {
		return fmt.Errorf("failed to create CNIServer directory: %v", err)
	}

	// Write config file
	config, err := json.Marshal(s.config)
	if err != nil {
		return fmt.Errorf("could not marshal config data: %v", err)
	}
	err = ioutil.WriteFile(configPath, config, 0444)
	if err != nil {
		return fmt.Errorf("could not write config file %q: %v", configPath, err)
	}

	// On Linux the socket is created with the permissions of the directory
	// it is in, so as long as the directory is root-only we can avoid
	// racy umask manipulation.
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on pod info socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		l.Close()
		return fmt.Errorf("failed to set pod info socket mode: %v", err)
	}

	s.SetKeepAlivesEnabled(false)
	go utilwait.Forever(func() {
		if err := s.Serve(l); err != nil {
			utilruntime.HandleError(fmt.Errorf("CNI server Serve() failed: %v", err))
		}
	}, 0)
	return nil
}

func ReadConfig(configPath string) (*Config, error) {
	bytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("OpenShift SDN network process is not (yet?) available")
		} else {
			return nil, fmt.Errorf("could not read config file %q: %v", configPath, err)
		}
	}
	var config Config
	if err = json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("could not parse config file %q: %v", configPath, err)
	}
	return &config, nil
}

// Split the "CNI_ARGS" environment variable's value into a map.  CNI_ARGS
// contains arbitrary key/value pairs separated by ';' and is for runtime or
// plugin specific uses.  Kubernetes passes the pod namespace and name in
// CNI_ARGS.
func gatherCNIArgs(env map[string]string) (map[string]string, error) {
	cniArgs, ok := env["CNI_ARGS"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_ARGS: '%s'", env)
	}

	mapArgs := make(map[string]string)
	for _, arg := range strings.Split(cniArgs, ";") {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid CNI_ARG '%s'", arg)
		}
		mapArgs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return mapArgs, nil
}

func cniRequestToPodRequest(r *http.Request) (*PodRequest, error) {
	var cr CNIRequest
	b, _ := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(b, &cr); err != nil {
		return nil, fmt.Errorf("JSON unmarshal error: %v", err)
	}

	cmd, ok := cr.Env["CNI_COMMAND"]
	if !ok {
		return nil, fmt.Errorf("unexpected or missing CNI_COMMAND")
	}

	req := &PodRequest{
		Command: CNICommand(cmd),
		Result:  make(chan *PodResult),
	}

	req.SandboxID, ok = cr.Env["CNI_CONTAINERID"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_CONTAINERID")
	}
	req.Netns, ok = cr.Env["CNI_NETNS"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_NETNS")
	}

	req.HostVeth = cr.HostVeth
	if req.HostVeth == "" && req.Command == CNI_ADD {
		return nil, fmt.Errorf("missing HostVeth")
	}

	cniArgs, err := gatherCNIArgs(cr.Env)
	if err != nil {
		return nil, err
	}

	req.PodNamespace, ok = cniArgs["K8S_POD_NAMESPACE"]
	if !ok {
		return nil, fmt.Errorf("missing K8S_POD_NAMESPACE")
	}

	req.PodName, ok = cniArgs["K8S_POD_NAME"]
	if !ok {
		return nil, fmt.Errorf("missing K8S_POD_NAME")
	}

	return req, nil
}

// Dispatch a pod request to the request handler and return the result to the
// CNI server client
func (s *CNIServer) handleCNIRequest(w http.ResponseWriter, r *http.Request) {
	req, err := cniRequestToPodRequest(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	klog.V(5).Infof("Waiting for %s result for pod %s/%s", req.Command, req.PodNamespace, req.PodName)
	result, err := s.requestFunc(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
	} else {
		// Empty response JSON means success with no body
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(result); err != nil {
			klog.Warningf("Error writing %s HTTP response: %v", req.Command, err)
		}
	}
}
