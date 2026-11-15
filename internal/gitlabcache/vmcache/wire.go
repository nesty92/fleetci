package vmcache

import "github.com/nesty92/fleetci/internal/cachelink"

// NodePort is where the VM reaches the node's signer: a port on the VM's
// loopback that the node's SSH connection forwards to it.
const NodePort = 31852

// VMPort is where the VM's service listens, on the VM's Docker bridge.
const VMPort = 31853

// SignReq is the VM's ask: "list" a prefix, or a link to "get" or "put"
// an object.
type SignReq struct {
	Op   string `json:"op"`
	Name string `json:"name"`
}

// SignResp is the node's answer: the listing's names, or the link.
type SignResp struct {
	Names []string        `json:"names,omitempty"`
	Link  *cachelink.Link `json:"link,omitempty"`
}
