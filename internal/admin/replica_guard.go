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

// replicaAction is the operation a replica description is for. It selects the
// warning wording; stop and restart are the guarded ones.
type replicaAction string

const (
	replicaActionStop    replicaAction = "stop"
	replicaActionRestart replicaAction = "restart"
	replicaActionDrain   replicaAction = "drain"
)

// replicaInfo describes the multi-host replica a node belongs to, as carried
// on the guard 409 and on the success response of a runtime stop/restart or
// a drain. It is nil (and the "replica" key absent) for a standalone node.
//
// Role and Head mirror the node list schedulingRole and replicaHead fields
// (same resolution, same wire strings). Role is always present; Head and
// Members are omitted when empty (an unresolved member has no head).
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
// The action selects the warning wording.
func describeReplicaMember(name string, action replicaAction, nodes []*router.NodeState, roles map[string]router.SchedulingRole, heads map[string]string) *replicaInfo {
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
	info.Warning = replicaWarning(name, action, role, info)
	return info
}

// replicaInfoFor resolves the replica description for one node against the
// router current fleet.
func (s *Server) replicaInfoFor(name string, action replicaAction) *replicaInfo {
	nodes := s.router.Nodes()
	roles, heads := s.router.SchedulingRolesWithHeads(nodes)
	return describeReplicaMember(name, action, nodes, roles, heads)
}

// replicaWarning states the topology facts for the action: what the node is
// in its replica and what the action does to it. It never claims that other
// members recover on their own. The members clause is omitted when the member
// list is empty rather than printed blank.
func replicaWarning(name string, action replicaAction, role router.SchedulingRole, info *replicaInfo) string {
	// memberList is " (members: a, b)" or "" when there are none.
	memberList := ""
	unresolvedList := " (members disagree on membership or head)"
	if len(info.Members) > 0 {
		list := strings.Join(info.Members, ", ")
		memberList = " (members: " + list + ")"
		unresolvedList = " (members disagree on membership or head; members: " + list + ")"
	}

	if action == replicaActionDrain {
		switch role {
		case router.RoleHead:
			return fmt.Sprintf("node %q is the head of a multi-host replica%s: draining it stops routing new requests to the whole replica.", name, memberList)
		case router.RoleWorker:
			return fmt.Sprintf("node %q is a worker in the multi-host replica headed by %q: draining a worker has no routing effect, because only the head receives requests. Drain the head %q to drain the replica.", name, info.Head, info.Head)
		default:
			return fmt.Sprintf("node %q is part of a multi-host replica declaration that does not resolve%s: its routing role cannot be determined until the declaration is fixed in the replica settings.", name, memberList)
		}
	}

	// Only stop and restart reach here (drain returned above, and the guard
	// never calls this for other actions).
	verb := "stopping"
	if action == replicaActionRestart {
		verb = "restarting"
	}
	switch role {
	case router.RoleHead:
		return fmt.Sprintf("node %q is the head of a multi-host replica%s: %s its runtime takes the whole replica offline. Marbor does not restart the other members.", name, memberList, verb)
	case router.RoleWorker:
		return fmt.Sprintf("node %q is a worker in the multi-host replica headed by %q%s: %s its runtime breaks that replica. Marbor does not restart or re-sync the other members.", name, info.Head, memberList, verb)
	default:
		return fmt.Sprintf("node %q is part of a multi-host replica declaration that does not resolve%s: the effect of %s its runtime cannot be determined. Fix the declaration in the replica settings first.", name, unresolvedList, verb)
	}
}

// replicaAcknowledged reports whether the request carries
// acknowledge_replica=true. Only that exact value counts.
func replicaAcknowledged(r *http.Request) bool {
	return r.URL.Query().Get(acknowledgeReplicaParam) == "true"
}

// guardReplicaRuntimeAction enforces the replica acknowledgement for a
// runtime stop or restart. It returns the replica description (nil for a
// standalone node or an action that is not guarded), whether the replica
// effect was acknowledged on this request (false for a standalone node), and
// whether the request may proceed. With proceed=false it has already written
// the 409 response.
func (s *Server) guardReplicaRuntimeAction(w http.ResponseWriter, r *http.Request, nodeName string, action replicaAction) (info *replicaInfo, acknowledged, proceed bool) {
	if action != replicaActionStop && action != replicaActionRestart {
		return nil, false, true
	}
	info = s.replicaInfoFor(nodeName, action)
	if info == nil {
		return nil, false, true
	}
	if replicaAcknowledged(r) {
		return info, true, true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   info.Warning + " Pass acknowledge_replica=true (CLI: --acknowledge-replica) to proceed; only the exact value acknowledge_replica=true is accepted.",
		"code":    replicaMemberCode,
		"replica": info,
	})
	return info, false, false
}

// replicaAuditDetail extends an audit detail string with the replica role and
// head when the node is a replica member.
func replicaAuditDetail(detail string, info *replicaInfo, acknowledged bool) string {
	if info == nil {
		return detail
	}
	// Quoted so a node name containing commas or newlines cannot forge extra
	// key: value fields in the audit detail.
	out := fmt.Sprintf("%s, Replica role: %q", detail, info.Role)
	if info.Head != "" {
		out += fmt.Sprintf(", Replica head: %q", info.Head)
	}
	if acknowledged {
		out += ", Replica acknowledged: true"
	}
	return out
}
