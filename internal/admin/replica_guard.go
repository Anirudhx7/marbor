package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Anirudhx7/marbor/internal/router"
)

const (
	// replicaMemberCode is the machine-readable "code" on the 409 returned
	// when a runtime stop/restart targets a multi-host replica head, worker
	// or unresolved member without acknowledging the effect.
	replicaMemberCode = "replica_member"

	// acknowledgeReplicaParam is the query parameter that acknowledges the
	// replica effect of a runtime stop/restart (CLI: --acknowledge-replica).
	acknowledgeReplicaParam = "acknowledge_replica"
)

// replicaInfo describes the multi-host replica a node belongs to, as carried
// on the guard's 409 and on the success response of a runtime stop/restart or
// a drain. It is nil (and the "replica" key absent) for a standalone node.
type replicaInfo struct {
	Role    string   `json:"role"`
	Head    string   `json:"head,omitempty"`
	Members []string `json:"members,omitempty"`
	Warning string   `json:"warning,omitempty"`
}

// describeReplicaMember returns the replica description for the named node,
// or nil when the node is standalone. roles/heads come from the fleet-wide
// resolution (router.SchedulingRolesWithHeads); the member list is derived
// from the same replica declarations and sorted so output is deterministic.
// The action ("stop", "restart" or "drain") selects the warning wording.
func describeReplicaMember(name, action string, nodes []*router.NodeState, roles map[string]router.SchedulingRole, heads map[string]string) *replicaInfo {
	role := roles[name]
	if role != router.RoleHead && role != router.RoleWorker && role != router.RoleUnresolved {
		return nil
	}

	info := &replicaInfo{Role: role.String()}
	if role != router.RoleUnresolved {
		info.Head = heads[name]
	}
	members, _ := router.ComponentFor(name, nodes)
	for _, m := range members {
		if m != nil {
			info.Members = append(info.Members, m.Name)
		}
	}
	sort.Strings(info.Members)
	info.Warning = replicaWarning(name, action, info)
	return info
}

// replicaInfoFor resolves the replica description for one node against the
// router's current fleet.
func (s *Server) replicaInfoFor(name, action string) *replicaInfo {
	nodes := s.router.Nodes()
	roles, heads := s.router.SchedulingRolesWithHeads(nodes)
	return describeReplicaMember(name, action, nodes, roles, heads)
}

// replicaWarning states the topology facts for the action: what the node is
// in its replica and what the action does to it. It never claims that other
// members recover on their own.
func replicaWarning(name, action string, info *replicaInfo) string {
	members := strings.Join(info.Members, ", ")
	switch action {
	case "drain":
		switch info.Role {
		case "head":
			return fmt.Sprintf("node %q is the head of a multi-host replica (members: %s): draining it stops routing new requests to the whole replica.", name, members)
		case "worker":
			return fmt.Sprintf("node %q is a worker in the multi-host replica headed by %q: draining a worker has no routing effect, because only the head receives requests. Drain the head %q to drain the replica.", name, info.Head, info.Head)
		default:
			return fmt.Sprintf("node %q is part of a multi-host replica declaration that does not resolve (members: %s): its routing role cannot be determined until the declaration is fixed on GPU Nodes.", name, members)
		}
	default:
		verb := action
		if action == "restart" {
			verb = "restarting"
		} else if action == "stop" {
			verb = "stopping"
		}
		switch info.Role {
		case "head":
			return fmt.Sprintf("node %q is the head of a multi-host replica (members: %s): %s its runtime takes the whole replica offline. Marbor does not restart the other members.", name, members, verb)
		case "worker":
			return fmt.Sprintf("node %q is a worker in the multi-host replica headed by %q (members: %s): %s its runtime breaks that replica. Marbor does not restart or re-sync the other members.", name, info.Head, members, verb)
		default:
			return fmt.Sprintf("node %q is part of a multi-host replica declaration that does not resolve (members disagree on membership or head; members: %s): the effect of %s its runtime cannot be determined. Fix the declaration on GPU Nodes first.", name, members, verb)
		}
	}
}

// replicaAcknowledged reports whether the request carries
// acknowledge_replica=true.
func replicaAcknowledged(r *http.Request) bool {
	return r.URL.Query().Get(acknowledgeReplicaParam) == "true"
}

// guardReplicaRuntimeAction enforces the replica acknowledgement for a
// runtime stop or restart. It returns the replica description (nil for a
// standalone node or an action that is not guarded) and whether the request
// may proceed. When it returns false it has already written the 409 response.
func (s *Server) guardReplicaRuntimeAction(w http.ResponseWriter, r *http.Request, nodeName, action string) (*replicaInfo, bool) {
	if action != "stop" && action != "restart" {
		return nil, true
	}
	info := s.replicaInfoFor(nodeName, action)
	if info == nil || replicaAcknowledged(r) {
		return info, true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   info.Warning + " Pass acknowledge_replica=true (CLI: --acknowledge-replica) to proceed.",
		"code":    replicaMemberCode,
		"replica": info,
	})
	return info, false
}

// replicaAuditDetail extends an audit detail string with the replica role and
// head when the node is a replica member.
func replicaAuditDetail(detail string, info *replicaInfo, acknowledged bool) string {
	if info == nil {
		return detail
	}
	out := fmt.Sprintf("%s, Replica role: %s", detail, info.Role)
	if info.Head != "" {
		out += ", Replica head: " + info.Head
	}
	if acknowledged {
		out += ", Replica acknowledged: true"
	}
	return out
}
