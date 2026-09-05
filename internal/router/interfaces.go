package router

// interfaces.go - Clean interfaces for routing decomposition.
//
// Defines empty interface shells for PredictiveEngine and PolicyEngine -
// extension points for later routing stages.

// PredictiveEngine will handle predictive prewarming of models based on
// past patterns, request history, and usage schedules.
type PredictiveEngine interface{}

// PolicyEngine will handle weighted placement scoring, determining routing weights
// based on node state, metrics, VRAM, and routing rules.
type PolicyEngine interface{}
