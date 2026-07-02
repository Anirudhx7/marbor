package router

// interfaces.go - Clean interfaces for routing decomposition.
//
// Defines empty interface shells for PredictiveEngine and PolicyEngine,
// paving the way for future steps (e.g. step 4 weighted placement scoring
// and step 5 predictive prewarming).

// PredictiveEngine will handle predictive prewarming of models based on
// past patterns, request history, and usage schedules.
type PredictiveEngine interface{}

// PolicyEngine will handle weighted placement scoring, determining routing weights
// based on node state, metrics, VRAM, and routing rules.
type PolicyEngine interface{}
