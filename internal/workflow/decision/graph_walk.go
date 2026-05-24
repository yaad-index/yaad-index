// CEL graph-walk primitives per ADR-0027 cut 3 (#232). Four
// functions × two arities (unfiltered + edge-type-filtered):
// graph.in_edges, graph.out_edges, graph.in_neighbors,
// graph.out_neighbors. All single-hop; multi-hop walking remains
// on /v1/entities/{id}/context?depth=N.
//
// Return shape per ADR-0027 §3 "List size cap + return-shape
// wrapping": every call returns a struct
// `{items, truncated, total}` so the truncation flag can ride
// alongside the data (CEL list<T> can't carry sidecar fields).

package decision

import (
	"context"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// DefaultGraphWalkCap is the per-call result cap when the operator
// hasn't overridden via `workflow.graph_walk_cap`. Sized for the
// day-anchor use case per ADR-0027 cut 3 (single day's inbound
// edges from one workflow's frontier).
const DefaultGraphWalkCap = 1000

// WalkEdge mirrors store.Edge for the CEL surface. Field names are
// lowercased per CEL convention (mirrors store.Edge.Metadata as
// `metadata`).
type WalkEdge struct {
	From     string         `cel:"from"`
	To       string         `cel:"to"`
	Type     string         `cel:"type"`
	Metadata map[string]any `cel:"metadata"`
}

// GraphWalker is the store-side surface the CEL graph-walk
// helpers dispatch through. Each method returns the bounded
// result slice + the unbounded total count, so the caller can
// surface `truncated: total > len(items)`. Empty edgeType means
// "no filter" — return every edge of any type at the requested
// endpoint.
//
// Production wires a store-backed implementation; tests substitute
// in-memory fakes. nil walker is tolerated by the CEL bindings
// (every helper returns an empty struct + no error) so unit tests
// of unrelated workflow logic don't need to stub the surface.
type GraphWalker interface {
	GraphLookup

	// InEdges returns edges terminating at toID, optionally
	// filtered by edge type. Truncated to `limit` entries; total
	// is the unbounded count.
	InEdges(ctx context.Context, toID, edgeType string, limit int) (edges []WalkEdge, total int, err error)
	// OutEdges returns edges originating at fromID.
	OutEdges(ctx context.Context, fromID, edgeType string, limit int) (edges []WalkEdge, total int, err error)
	// InNeighbors returns the source-side entities of edges
	// terminating at toID. Each entity is a dynamic map matching
	// the GraphLookup.Get shape.
	InNeighbors(ctx context.Context, toID, edgeType string, limit int) (entities []map[string]any, total int, err error)
	// OutNeighbors returns the target-side entities of edges
	// originating at fromID.
	OutNeighbors(ctx context.Context, fromID, edgeType string, limit int) (entities []map[string]any, total int, err error)
}

// walkResult assembles the `{items, truncated, total}` map cel-go
// returns to workflow CEL. Common shape for both edge and neighbor
// walks.
func walkResult(items []ref.Val, total, limit int) ref.Val {
	out := map[string]any{
		"items":     items,
		"truncated": total > len(items),
		"total":     total,
	}
	return types.DefaultTypeAdapter.NativeToValue(out)
}

// edgesToRefVals converts a []WalkEdge slice into the []ref.Val
// shape cel-go expects for list<map>. Each entry becomes a CEL
// map with from/to/type/metadata keys.
func edgesToRefVals(edges []WalkEdge) []ref.Val {
	out := make([]ref.Val, len(edges))
	for i, e := range edges {
		m := map[string]any{
			"from":     e.From,
			"to":       e.To,
			"type":     e.Type,
			"metadata": e.Metadata,
		}
		out[i] = types.DefaultTypeAdapter.NativeToValue(m)
	}
	return out
}

// entitiesToRefVals converts a []map[string]any slice into the
// []ref.Val shape cel-go expects for list<map>.
func entitiesToRefVals(entities []map[string]any) []ref.Val {
	out := make([]ref.Val, len(entities))
	for i, e := range entities {
		out[i] = types.DefaultTypeAdapter.NativeToValue(e)
	}
	return out
}

// graphWalkFunctions returns the cel.EnvOption blocks that
// register the eight graph-walk overloads. Each binding closes
// over the evaluator so it can dispatch through e.walker (set
// via Options.Walker at NewEvaluator time) using the per-eval
// context (e.currentCtx, guarded by evalMu).
func (e *Evaluator) graphWalkFunctions() []cel.EnvOption {
	mapType := cel.MapType(cel.StringType, cel.DynType)
	listOfMaps := cel.ListType(mapType)
	resultType := cel.MapType(cel.StringType, cel.DynType)

	emptyResult := func() ref.Val {
		return walkResult([]ref.Val{}, 0, e.graphWalkCap)
	}

	walkEdgesUnary := func(walkFn func(ctx context.Context, id, edgeType string, limit int) ([]WalkEdge, int, error)) func(arg ref.Val) ref.Val {
		return func(arg ref.Val) ref.Val {
			if e.walker == nil {
				return emptyResult()
			}
			id, ok := arg.Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string id, got %T", arg.Value())
			}
			edges, total, err := walkFn(e.currentCtx, id, "", e.graphWalkCap)
			if err != nil {
				return types.NewErr("graph-walk: %v", err)
			}
			return walkResult(edgesToRefVals(edges), total, e.graphWalkCap)
		}
	}
	walkEdgesBinary := func(walkFn func(ctx context.Context, id, edgeType string, limit int) ([]WalkEdge, int, error)) func(args ...ref.Val) ref.Val {
		return func(args ...ref.Val) ref.Val {
			if e.walker == nil {
				return emptyResult()
			}
			id, ok := args[0].Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string id, got %T", args[0].Value())
			}
			edgeType, ok := args[1].Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string edge_type, got %T", args[1].Value())
			}
			edges, total, err := walkFn(e.currentCtx, id, edgeType, e.graphWalkCap)
			if err != nil {
				return types.NewErr("graph-walk: %v", err)
			}
			return walkResult(edgesToRefVals(edges), total, e.graphWalkCap)
		}
	}
	walkNeighborsUnary := func(walkFn func(ctx context.Context, id, edgeType string, limit int) ([]map[string]any, int, error)) func(arg ref.Val) ref.Val {
		return func(arg ref.Val) ref.Val {
			if e.walker == nil {
				return emptyResult()
			}
			id, ok := arg.Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string id, got %T", arg.Value())
			}
			entities, total, err := walkFn(e.currentCtx, id, "", e.graphWalkCap)
			if err != nil {
				return types.NewErr("graph-walk: %v", err)
			}
			return walkResult(entitiesToRefVals(entities), total, e.graphWalkCap)
		}
	}
	walkNeighborsBinary := func(walkFn func(ctx context.Context, id, edgeType string, limit int) ([]map[string]any, int, error)) func(args ...ref.Val) ref.Val {
		return func(args ...ref.Val) ref.Val {
			if e.walker == nil {
				return emptyResult()
			}
			id, ok := args[0].Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string id, got %T", args[0].Value())
			}
			edgeType, ok := args[1].Value().(string)
			if !ok {
				return types.NewErr("graph-walk: expected string edge_type, got %T", args[1].Value())
			}
			entities, total, err := walkFn(e.currentCtx, id, edgeType, e.graphWalkCap)
			if err != nil {
				return types.NewErr("graph-walk: %v", err)
			}
			return walkResult(entitiesToRefVals(entities), total, e.graphWalkCap)
		}
	}

	inEdgesFn := func(ctx context.Context, id, edgeType string, limit int) ([]WalkEdge, int, error) {
		return e.walker.InEdges(ctx, id, edgeType, limit)
	}
	outEdgesFn := func(ctx context.Context, id, edgeType string, limit int) ([]WalkEdge, int, error) {
		return e.walker.OutEdges(ctx, id, edgeType, limit)
	}
	inNeighborsFn := func(ctx context.Context, id, edgeType string, limit int) ([]map[string]any, int, error) {
		return e.walker.InNeighbors(ctx, id, edgeType, limit)
	}
	outNeighborsFn := func(ctx context.Context, id, edgeType string, limit int) ([]map[string]any, int, error) {
		return e.walker.OutNeighbors(ctx, id, edgeType, limit)
	}

	_ = listOfMaps
	return []cel.EnvOption{
		cel.Function("graph.in_edges",
			cel.Overload("graph_in_edges_string",
				[]*cel.Type{cel.StringType},
				resultType,
				cel.UnaryBinding(walkEdgesUnary(inEdgesFn)),
			),
			cel.Overload("graph_in_edges_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				resultType,
				cel.FunctionBinding(walkEdgesBinary(inEdgesFn)),
			),
		),
		cel.Function("graph.out_edges",
			cel.Overload("graph_out_edges_string",
				[]*cel.Type{cel.StringType},
				resultType,
				cel.UnaryBinding(walkEdgesUnary(outEdgesFn)),
			),
			cel.Overload("graph_out_edges_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				resultType,
				cel.FunctionBinding(walkEdgesBinary(outEdgesFn)),
			),
		),
		cel.Function("graph.in_neighbors",
			cel.Overload("graph_in_neighbors_string",
				[]*cel.Type{cel.StringType},
				resultType,
				cel.UnaryBinding(walkNeighborsUnary(inNeighborsFn)),
			),
			cel.Overload("graph_in_neighbors_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				resultType,
				cel.FunctionBinding(walkNeighborsBinary(inNeighborsFn)),
			),
		),
		cel.Function("graph.out_neighbors",
			cel.Overload("graph_out_neighbors_string",
				[]*cel.Type{cel.StringType},
				resultType,
				cel.UnaryBinding(walkNeighborsUnary(outNeighborsFn)),
			),
			cel.Overload("graph_out_neighbors_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				resultType,
				cel.FunctionBinding(walkNeighborsBinary(outNeighborsFn)),
			),
		),
	}
}

var _ = fmt.Sprintf
