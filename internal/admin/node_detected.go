package admin

import "github.com/Anirudhx7/marbor/internal/marboragent"
import "github.com/Anirudhx7/marbor/internal/router"

// nodeDetectedExtras is embedded in nodeResp (its fields are flattened into
// the node JSON). It carries what the agent detected beyond the original
// type/width/group trio.
//
// DetectedPipelineWidth/DetectedDataWidth are the secondary degrees of a
// launch that sets pipeline or data parallelism alongside tensor parallel.
// DetectedGPUScope is the agent's evidence for which GPUs the runtime uses
// and how it was learned; absent when unknown (older agent, or the agent
// could not tell). DetectedDrivesPlacement is true only when a verified
// detection (an independent per-process check agreed) is what constrains
// placement; otherwise detected values are informational until the operator
// adopts them.
type nodeDetectedExtras struct {
	DetectedPipelineWidth   int                   `json:"detectedPipelineWidth,omitempty"`
	DetectedDataWidth       int                   `json:"detectedDataWidth,omitempty"`
	DetectedGPUScope        *detectedGPUScopeResp `json:"detectedGPUScope,omitempty"`
	DetectedDrivesPlacement bool                  `json:"detectedDrivesPlacement"`
}

// detectedGPUScopeResp is the node API's view of the agent's GPU scope
// evidence. Field names follow the camelCase convention of the node API.
type detectedGPUScopeResp struct {
	Indices      []int    `json:"indices,omitempty"`
	UUIDs        []string `json:"uuids,omitempty"`
	Raw          string   `json:"raw,omitempty"`
	Source       string   `json:"source,omitempty"`
	CrossChecked bool     `json:"crossChecked"`
	Note         string   `json:"note,omitempty"`
}

func newNodeDetectedExtras(n *router.NodeState) nodeDetectedExtras {
	pipeline, data := n.DetectedSecondaryWidths()
	return nodeDetectedExtras{
		DetectedPipelineWidth:   pipeline,
		DetectedDataWidth:       data,
		DetectedGPUScope:        toDetectedGPUScopeResp(n.DetectedGPUScopeCopy()),
		DetectedDrivesPlacement: n.DetectedDrivesPlacement(),
	}
}

func toDetectedGPUScopeResp(s *marboragent.GPUScope) *detectedGPUScopeResp {
	if s == nil {
		return nil
	}
	return &detectedGPUScopeResp{
		Indices:      s.Indices,
		UUIDs:        s.UUIDs,
		Raw:          s.Raw,
		Source:       s.Source,
		CrossChecked: s.CrossChecked,
		Note:         s.Note,
	}
}
