package api

import (
	"net/http"
	"net/netip"
	"time"

	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.Nodes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]types.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.node(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.Node(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.node(*n))
}

type nodePatch struct {
	Schedulable *bool `json:"schedulable,omitempty"`
	Ingress     *bool `json:"ingress,omitempty"`
	Draining    *bool `json:"draining,omitempty"`
}

func (s *Server) patchNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var p nodePatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	for flag, v := range map[string]*bool{"schedulable": p.Schedulable, "ingress": p.Ingress, "draining": p.Draining} {
		if v == nil {
			continue
		}
		if err := s.Store.SetNodeFlag(r.Context(), name, flag, *v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.getNode(w, r)
}

func (s *Server) node(n store.Node) types.Node {
	subnet, meshIP := NodeSubnet(s.ClusterCIDR, n.SubnetIndex)
	return types.Node{
		Name:        n.Name,
		Role:        n.Role,
		Status:      n.Status(time.Now()),
		Address:     n.Address,
		MeshIP:      meshIP.String(),
		Subnet:      subnet.String(),
		Arch:        n.Arch,
		CPUs:        n.CPUs,
		MemoryMB:    n.MemoryMB,
		Schedulable: n.Schedulable,
		Ingress:     n.Ingress,
		LastSeen:    n.LastSeen,
		CreatedAt:   n.CreatedAt,
	}
}

// NodeSubnet returns node index's /24 inside a /16 cluster network and the
// node's own address (.1) in it: index 3 in 10.210.0.0/16 is 10.210.3.0/24
// and 10.210.3.1.
func NodeSubnet(cluster netip.Prefix, index int) (netip.Prefix, netip.Addr) {
	b := cluster.Masked().Addr().As4()
	b[2] = byte(index)
	subnet := netip.PrefixFrom(netip.AddrFrom4(b), 24)
	b[3] = 1
	return subnet, netip.AddrFrom4(b)
}
