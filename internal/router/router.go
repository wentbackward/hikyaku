// Package router resolves virtual model names to concrete backends and
// applies per-model parameter profiles (defaults → caller → clamp).
package router

import (
	"fmt"

	"github.com/wentbackward/hikyaku/internal/config"
)

// samplingKeys is the set of parameters that may be overridden by route profiles.
var samplingKeys = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true,
	"max_tokens": true, "max_completion_tokens": true,
	"presence_penalty":  true,
	"frequency_penalty": true, "seed": true, "stop": true,
	"enable_thinking": true, "thinking_budget": true,
}

// Resolution is the output of routing: a concrete backend, the real model
// name to send upstream, and the merged sampling parameters.
type Resolution struct {
	Backend   *config.Backend
	Route     *config.Route // source route; nil for passthrough
	RealModel string
	// Params contains only sampling parameters — callers must merge these
	// back into the request body themselves.
	Params map[string]interface{}
	// SystemPrompt is the route's optional system-prompt mutation.
	// IsZero() reports whether any mutation was requested.
	SystemPrompt config.SystemPromptOp
	// Inject is the route's optional body deep-merge map. Nil if unset.
	Inject map[string]interface{}
	// Headers is the route's optional outbound header manipulation. Applied
	// after backend.Headers so route wins on conflict.
	Headers config.HeadersOp
	// Group is the LB group name when the route uses backend_group.
	// Empty string means single-backend mode (existing behavior).
	Group string
}

// Router resolves virtual model names.
type Router struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Router {
	return &Router{cfg: cfg}
}

// Resolve returns a Resolution for the given virtual model name, or an error
// if the model is unknown. body should be the decoded request body; sampling
// params present in it participate in the merge.
func (r *Router) Resolve(modelName string, body map[string]interface{}) (*Resolution, error) {
	return r.resolve(modelName, body, 0)
}

func (r *Router) resolve(modelName string, body map[string]interface{}, depth int) (*Resolution, error) {
	if depth > 1 {
		return nil, fmt.Errorf("routing cycle: auto_route chains more than one level deep (model %q)", modelName)
	}

	route, ok := r.cfg.Route(modelName)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelName)
	}

	// Auto-routing: inspect message content then recurse once.
	if route.AutoRoute != nil {
		messages, _ := body["messages"].([]interface{})
		target := route.AutoRoute.Text
		if IsMultimodal(messages) {
			target = route.AutoRoute.Vision
		}
		return r.resolve(target, body, depth+1)
	}

	var backend *config.Backend
	var group string

	if route.BackendGroup != "" {
		// LB group route — backend is selected by the Balancer at runtime.
		// Pick the first backend for model resolution (real_model, etc.).
		group = route.BackendGroup
		backends := r.cfg.GroupBackends(route.BackendGroup)
		if len(backends) == 0 {
			return nil, fmt.Errorf("route %q: group %q has no backends", modelName, route.BackendGroup)
		}
		backend = backends[0]
	} else {
		// Single-backend route (existing path)
		var ok bool
		backend, ok = r.cfg.Backend(route.Backend)
		if !ok {
			return nil, fmt.Errorf("route %q references unknown backend %q", modelName, route.Backend)
		}
	}

	params := mergeParams(route.Defaults, body, route.Clamp)

	// Fall back to the route's canonical name, not the requested one: when
	// the request arrived via an alias, the upstream only knows the model
	// the route actually names. Identical for non-alias requests.
	realModel := route.RealModel
	if realModel == "" {
		realModel = route.VirtualModel
	}

	return &Resolution{
		Backend:      backend,
		Route:        route,
		RealModel:    realModel,
		Params:       params,
		SystemPrompt: route.SystemPrompt,
		Inject:       route.Inject,
		Headers:      route.Headers,
		Group:        group,
	}, nil
}

// mergeParams applies the three-layer merge: defaults < caller < clamp.
func mergeParams(defaults, body, clamp map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(defaults)+len(clamp))

	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range body {
		if samplingKeys[k] {
			out[k] = v
		}
	}
	// Clamp always wins — caller cannot override.
	for k, v := range clamp {
		out[k] = v
	}
	return out
}

// IsMultimodal returns true if any message contains a non-text content part
// (image, video, document, file).
func IsMultimodal(messages []interface{}) bool {
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]interface{})
		if !ok {
			continue // string content → text only
		}
		for _, p := range parts {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "image_url", "image", "video_url", "video", "document", "file":
				return true
			}
		}
	}
	return false
}
